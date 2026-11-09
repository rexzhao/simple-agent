plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
    id("org.jetbrains.kotlin.plugin.compose")
    id("org.jetbrains.kotlin.plugin.serialization")
}

android {
    namespace = "com.simpleagent.viewer"
    compileSdk = 35
    defaultConfig {
        applicationId = "com.simpleagent.viewer"
        // Android 8.0：gomobile 的 AAR 用 JNI，24 是它的默认下限
        minSdk = 24
        targetSdk = 35
        versionCode = 1
        versionName = "0.1.0"
    }

    buildFeatures {
        compose = true
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
    }

    buildTypes {
        release {
            isMinifyEnabled = false
        }
    }
}

dependencies {
    // Go 内核（由 scripts/build-android.ps1 生成，见下面 bindGoKernel 任务）
    implementation(files("libs/bridge.aar"))

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
