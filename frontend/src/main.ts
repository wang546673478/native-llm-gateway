import { createApp } from 'vue'
import { createPinia } from 'pinia'
import App from './App.vue'
import router from './router'
// tokens.css 必须最先引入:它定义全局 CSS 变量 + body 基线样式,
// 且 utils/status.ts 的 STATUS_PALETTE 在运行时 getComputedStyle 读这些变量。
import './styles/tokens.css'

const app = createApp(App)
app.use(createPinia())
app.use(router)
app.mount('#app')
