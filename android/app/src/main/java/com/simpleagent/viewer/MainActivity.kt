package com.simpleagent.viewer

import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.activity.viewModels
import com.simpleagent.viewer.ui.ViewerScreen
import com.simpleagent.viewer.ui.ViewerTheme
import com.simpleagent.viewer.ui.ViewerViewModel

class MainActivity : ComponentActivity() {

    private val viewModel: ViewerViewModel by viewModels { ViewerViewModel.Factory }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        // 调试/自动化入口：`adb shell am start -e addr HOST:PORT -e route ID ...`
        // 直接预填连接参数，不必在手机键盘上敲地址（中文输入法下尤其麻烦）。
        // 生产用户不会带这些 extra，行为不变。
        intent?.getStringExtra("addr")?.let(viewModel::updateAddr)
        intent?.getStringExtra("route")?.let(viewModel::updateRoute)
        intent?.getStringExtra("token")?.let(viewModel::updateToken)

        enableEdgeToEdge()
        setContent {
            ViewerTheme {
                ViewerScreen(viewModel = viewModel)
            }
        }
    }
}
