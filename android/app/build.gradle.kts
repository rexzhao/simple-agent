plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
    id("org.jetbrains.kotlin.plugin.compose")
    id("org.jetbrains.kotlin.plugin.serialization")
}

android {
    namespace = "com.simpleagent.viewer"
    compileSdk = 35

    // 版本号：优先用环境变量（CI 从 tag 推导），其次自动推一个**单调递增**的值。
    //
    // 为什么不能写死：写死的后果是装到手机上全显示同一个版本，分不清装的是
    // 哪个包；更糟的是 versionCode 比不上已装的包，adb install 会直接拒绝
    // （INSTALL_FAILED_VERSION_DOWNGRADE）。
    //
    // 本地构建（没设环境变量）时用"距 2026-01-01 的天数×10000 + 当天分钟数"。
    // 只要当前时间比上一个包晚，code 就更大 —— 满足安卓"覆盖安装"的要求。
    // CI 用同一套公式（见 .github/workflows/android.yml），两边出的包才能互相覆盖。
    val envVersionName = System.getenv("SIMPLE_AGENT_VERSION_NAME")
    val envVersionCode = System.getenv("SIMPLE_AGENT_VERSION_CODE")?.toIntOrNull()

    val autoVersionName = envVersionName ?: run {
        val stamp = java.text.SimpleDateFormat("yyyyMMdd.HHmm")
            .format(java.util.Date())
        "0.1.8-dev.$stamp"
    }
    val autoVersionCode = envVersionCode ?: run {
        // 距 2026-01-01 的天数 × 10000 + 当天分钟数（0..1439），
        // 保证"后构建的包 code 更大"。上限约 3.6e9，int 放得下。
        val base = java.time.LocalDate.of(2026, 1, 1)
        val now = java.time.LocalDate.now()
        val days = java.time.temporal.ChronoUnit.DAYS.between(base, now).toInt()
        val minuteOfDay = java.time.LocalTime.now().let { it.hour * 60 + it.minute }
        days * 10000 + minuteOfDay
    }

    defaultConfig {
        applicationId = "com.simpleagent.viewer"
        // Android 8.0：gomobile 的 AAR 用 JNI，24 是它的默认下限
        minSdk = 24
        targetSdk = 35
        // 本地直接构建（没有环境变量）时用这组默认值。
        versionCode = autoVersionCode
        versionName = autoVersionName
    }

    buildFeatures {
        compose = true
        // 生成 BuildConfig：调试入口（一键自动发消息的按钮）只在 Debug 构建显示，
        // 用 BuildConfig.DEBUG 判断，不要把这个按钮带进正式版。
        buildConfig = true
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    kotlinOptions {
        jvmTarget = "17"
    }

    packaging {
        resources {
            excludes += "/META-INF/{AL2.0,LGPL2.1}"
        }
        jniLibs {
            // 16 KB 页大小支持（Android 15+）。AGP 8.5.1+ 会在未压缩存储 .so
            // 时按 16 KB 边界对齐 zip 条目；useLegacyPackaging=false 是前提，
            // 打开它 .so 才是未压缩 + 对齐的。
            useLegacyPackaging = false
        }
    }

    buildTypes {
        release {
            isMinifyEnabled = false
        }
        debug {
            // 用环境里的 keystore 签名（CI / 本机固定签名都用它）。
            //
            // 为什么需要：AGP 默认用 <用户>/.android/debug.keystore，
            // 而 GitHub runner 每次都是全新环境，那个文件是随机生成的——
            // 于是同一个 App 在 CI 与本地构建出来的包签名不同，
            // 手机上覆盖安装会报 INSTALL_FAILED_UPDATE_INCOMPATIBLE。
            //
            // 这里允许用环境变量指定同一份 keystore，保证任何地方出的包
            // 签名一致。没有指定时保持 AGP 默认行为（不影响普通开发）。
            val storePath = System.getenv("SIMPLE_AGENT_KEYSTORE")
            if (!storePath.isNullOrEmpty()) {
                signingConfig = signingConfigs.create("fromEnv") {
                    storeFile = rootProject.file(storePath)
                    storePassword = System.getenv("SIMPLE_AGENT_KEYSTORE_PASSWORD")
                        ?: "android"
                    keyAlias = System.getenv("SIMPLE_AGENT_KEY_ALIAS")
                        ?: "androiddebugkey"
                    keyPassword = System.getenv("SIMPLE_AGENT_KEY_PASSWORD")
                        ?: System.getenv("SIMPLE_AGENT_KEYSTORE_PASSWORD")
                        ?: "android"
                }
            }
        }
    }
}

dependencies {
    // Go 内核（由 scripts/build-android.ps1 生成，见下面 bindGoKernel 任务）
    implementation(files("libs/bridge.aar"))

    // 强制升级 graphics-path：Compose UI 1.7.6 传递依赖的是 1.0.1，
    // 该版本的 libandroidx.graphics.path.so 在 16 KB 页设备上会被
    // Android 15 的兼容性检查报错（"未知错误"）。1.1.0 已按 16 KB 重新编译。
    implementation("androidx.graphics:graphics-path:1.1.0")

    implementation("androidx.core:core-ktx:1.15.0")
    implementation("androidx.activity:activity-compose:1.9.3")
    implementation("androidx.lifecycle:lifecycle-runtime-compose:2.8.7")
    implementation("androidx.lifecycle:lifecycle-viewmodel-compose:2.8.7")

    val composeBom = platform("androidx.compose:compose-bom:2024.12.01")
    implementation(composeBom)
    implementation("androidx.compose.ui:ui")
    implementation("androidx.compose.ui:ui-tooling-preview")
    implementation("androidx.compose.foundation:foundation")
    implementation("androidx.compose.material3:material3")
    implementation("androidx.compose.material:material-icons-extended")

    implementation("org.jetbrains.kotlinx:kotlinx-serialization-json:1.7.3")
    implementation("org.jetbrains.kotlinx:kotlinx-coroutines-android:1.9.0")

    debugImplementation("androidx.compose.ui:ui-tooling")
}

// ---- Go 内核（gomobile AAR）----
//
// AAR 是**生成物**，但它很容易过期：改了 internal/* 里的 Go 代码后忘了重新 bind，
// APK 里跑的还是旧逻辑（我们踩过一次：relay token 的修复没进 APK，表现为
// "token 明明对却被拒绝"）。所以把 bind 接进构建流程：mobile/ 或 internal/ 下的
// .go 变了就自动重新生成 AAR。
//
// 需要 gomobile + NDK；缺工具链的环境（例如只跑 Kotlin 编译）只告警、不中断。
val repoRoot: File = rootProject.projectDir.parentFile
val goSources = fileTree(repoRoot) {
    include("mobile/**/*.go", "internal/**/*.go", "go.mod", "go.sum")
    exclude("**/*_test.go")
}
val aarFile = file("libs/bridge.aar")

val bindGoKernel = tasks.register<Exec>("bindGoKernel") {
    workingDir = repoRoot
    val winGomobile = File(System.getProperty("user.home"), "go/bin/gomobile.exe")
    val unixGomobile = File(System.getProperty("user.home"), "go/bin/gomobile")
    commandLine(
        if (winGomobile.exists()) winGomobile.absolutePath else unixGomobile.absolutePath,
        "bind",
        "-target=android/arm64,android/arm,android/amd64,android/386",
        "-androidapi=24",
        // Go 默认按 4 KB 页对齐 ELF 的 PT_LOAD 段，Android 15+ 的 16 KB 页
        // 设备会拒绝加载，安装后弹"此应用不符合 16 KB 对齐要求"。
        // 这个参数把对齐改成 16 KB；只影响段对齐，不影响功能。
        // 改这里的同时要改 scripts/build-android.sh 和 build-android.ps1，
        // 三处都是 gomobile bind 的入口。
        "-ldflags=-extldflags=-Wl,-z,max-page-size=16384",
        // 先写临时文件再替换：gomobile 会先**清空** -o 指定的文件再写入，
        // 直接指向 libs/bridge.aar 的话，Gradle 有概率读到 0 字节的 AAR
        // （表现为 "zip file is empty"，或 APK 里没有 libgojni.so）。
        // 临时文件名必须以 .aar 结尾（gomobile 会校验扩展名）。
        "-o", File(buildDir, "go-kernel/bridge-build.aar").absolutePath,
        "./mobile/bridge",
    )
    inputs.files(goSources)
    outputs.file(aarFile)
    isIgnoreExitValue = true
    // gomobile 不会自己建输出目录（第一次构建时会以
    // "open .../go-kernel/bridge-build.aar: The system cannot find the path specified" 失败）
    doFirst {
        File(buildDir, "go-kernel").mkdirs()
    }
    // gomobile 最后一步要用 javac 生成 Java 绑定：必须把 JDK 放进 PATH，
    // 否则会报 `exec: "javac": executable file not found in %PATH%`。
    // Java 15+ 不再有 jre 目录，javac 就在 JAVA_HOME/bin 下。
    val javaHome = System.getProperty("java.home")
    val jdkBin = File(javaHome, "bin")
    environment("PATH", jdkBin.absolutePath + File.pathSeparator + System.getenv("PATH"))
    environment("JAVA_HOME", javaHome)
    doLast {
        if (executionResult.get().exitValue != 0) {
            logger.warn("gomobile bind 失败：沿用已有的 libs/bridge.aar")
            return@doLast
        }
        val produced = File(buildDir, "go-kernel/bridge-build.aar")
        if (produced.exists() && produced.length() > 0) {
            produced.copyTo(aarFile, overwrite = true)
        }
    }
}

// 让"打包 so"这一步依赖 bind 结果：AAR 过期时会先重新 bind。
// 只在 AAR 已存在时接上，避免首次 clone（还没生成过 AAR）时把构建卡在工具链上。
tasks.matching { it.name == "mergeDebugJniLibFolders" || it.name == "mergeReleaseJniLibFolders" }
    .configureEach {
        if (aarFile.exists()) dependsOn(bindGoKernel)
    }
