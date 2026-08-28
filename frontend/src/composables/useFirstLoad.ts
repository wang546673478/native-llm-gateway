import { ref, watch, type Ref } from 'vue'

// useFirstLoad 从已有的 loading 派生「是否还没加载完第一次」。
//
// 为什么做成 composable 而不是每个 view 自己加一个 ref:
// 8 个表格页都要「首屏骨架、之后刷新只转圈」这一个行为,各写一份就是
// 8 处重复(改一次语义要改 8 个文件)。这里只观察 loading 的 true→false 跃变,
// **不碰各 view 的 load() 函数体** —— 那些函数里有各自的业务逻辑,不该为了
// 加骨架去动它们。
//
// 用法:
//   const { firstLoad } = useFirstLoad(loading)
//   <n-spin :show="loading && !firstLoad">
//     <table-skeleton v-if="firstLoad" /><n-data-table v-else ... />
export function useFirstLoad(loading: Ref<boolean>): { firstLoad: Ref<boolean> } {
  const firstLoad = ref(true)
  // 只认「结束」那一刻:loading 从 true 落回 false 说明第一批数据到位了。
  // 有些 view 的 loading 初值是 false(load 里才置 true),所以不能用初值判断。
  const stop = watch(loading, v => {
    if (!v) {
      firstLoad.value = false
      stop() // 一次性,之后的刷新不再关心
    }
  })
  return { firstLoad }
}
