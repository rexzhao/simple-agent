# Android 端显示图片（执行端 blob）

> 给接手的 agent：想在 Android 端把执行端的图片显示出来，照这份做。
> 现有实现已完整可用，新增展示位置时**复用**这些设施，不要另造一套。

---

## 一句话

**timeline 里只传 `sha256` 引用；Android 拿 hash 向执行端要原始字节，
校验后落盘到本地，再用本地文件路径显示。**

---

## 数据流

```
模型工具产出图片
  ↓
执行端存 <data_dir>/blobs/<hash 前 2 位>/<完整 hash>   ← 内容寻址，无扩展名
  ↓
timeline JSON 里的 part 只带引用：
  {"kind":"image",
   "image":{"blob":"sha256:xxxx","url":"","media_type":"image/png",
            "width":0,"height":0}}
  ↓
Android：hash → CoreClient.blobPathAsync() → Go 侧 client.blobPath(hash)
  ↓
Go 向执行端要字节 → 校验 sha256 → 落盘到 Android 本地缓存
  ↓
返回本地文件路径 → Compose 按路径显示
```

---

## 关键代码位置

| 层 | 文件 | 作用 |
| --- | --- | --- |
| 数据模型 | `android/.../data/Models.kt` 的 `ImageView` | `blob`（hash）、`url`、`mediaType`、`width`、`height` |
| 请求入口 | `android/.../ui/ViewerViewModel.kt` 的 `imagePath(hash, onReady)` | 暴露给 UI |
| 线程切换 | `android/.../data/CoreClient.kt` 的 `blobPathAsync` | **必须切 IO**，底层阻塞 |
| Go 实现 | `mobile/bridge/bridge.go` 的 `Client.BlobPath` | 取字节 + 校验 + 落盘 |
| 本地缓存 | `mobile/bridge/infra.go` 的 `saveBlob` | 原子写（temp + rename） |
| UI 渲染 | `android/.../ui/Timeline.kt` 的 `ImagePart` | 异步取路径，失败显示占位 |
| 放大查看 | `android/.../ui/ViewerScreen.kt` 的 `ZoomableImageOrPlaceholder` | 加载 + 占位 + 缩放 |

---

## 必须遵守的四条

### 1. timeline 里绝不内联图片字节

一张图几 MB。分页一次 60 条，可能几十张图 —— 内联会把 JSON 撑到几百 MB，
首屏直接不可用。

### 2. 取图必须放后台线程

`blobPath` 是**阻塞**调用（等执行端回包 + 磁盘写入）：

```kotlin
fun blobPathAsync(hash: String, onResult: (String) -> Unit) {
    io.execute {
        val path = client.blobPath(hash)   // 阻塞
        main.post { if (!closed.get()) onResult(path) }
    }
}
```

放主线程会卡死 UI。

### 3. 校验 sha256，不通过就当失败

```go
sum := sha256.Sum256(data)
if got := hex.EncodeToString(sum[:]); got != hash {
    c.setError(fmt.Sprintf("图片校验失败：收到 %d 字节，与 hash 不符", len(data)))
    return ""
}
```

理由：空数据或错内容会让界面显示一张**坏图**，比"图片丢失"难排查得多。

### 4. 失败要降级，不能崩

```kotlin
onImagePath(blob) { result ->
    if (result.isEmpty()) failed = true else path = result
}
```

失败显示占位符（`ImagePart` 里的 `failed` 分支）。

---

## 内容寻址的三个附带好处

- **文件名就是 hash**：重复写入无副作用；先查缓存可直接返回，同一张图
  看第二次不走网络。
- **原子写**：先写 `<path>.tmp` 再 `rename`。中途失败不会留下半个文件
  被当成有效缓存（`infra.go` 的 `saveBlob`）。
- **用户上传的附件走同一条路**：`putBlobAsync(bytes)` 拿到 hash，
  随消息一起发出。发送端和展示端共用同一套 blob 设施。

---

## 新增展示位置怎么做

比如想在归档消息、工具输出里显示图片，照抄这两步：

```kotlin
// 1. 从 part 拿 hash
val hash = part.image?.blob.orEmpty()

// 2. 异步取本地路径
remember(hash) {
    viewModel.imagePath(hash) { path = it }
}
```

`ZoomableImageOrPlaceholder` 已封装加载 + 占位 + 缩放，
直接把 `onImagePath` 回调传进去即可。

---

## 调试

```bash
# 执行端/本地缓存里有哪些图（文件名就是 sha256）
adb shell run-as com.simpleagent.viewer ls cache/blobs/

# 取某张图看内容是否正常
adb shell run-as com.simpleagent.viewer cat cache/blobs/<完整 hash> > /tmp/img.png

# 失败原因
adb logcat -d -s SimpleAgent | grep -iE "图片|blob"
```

`BlobPath` 失败时会把原因写进 `client.lastError()`，
Android 侧通过 `CoreClient.lastError()` 读。

---

## 常见坑

| 症状 | 原因 |
| --- | --- |
| 界面一直转圈不显示 | `onImagePath` 在主线程调用了阻塞的 `blobPath` |
| 提示"图片校验失败" | 执行端回的字节与 hash 不匹配（数据损坏或 hash 传错） |
| 显示一张坏图 | 没做校验就用了执行端返回的字节 |
| 同一张图反复加载 | 每次重建 `remember` 的 key（应锚定 hash 而非整个 part） |
| 附件发出去但对方看不到 | `putBlobAsync` 未完成就发送了（要等回调拿到 hash 再发） |