// 分页状态 + 处理器的高内聚封装。
//
// 低耦合:Usage.vue 与 AccessLogs.vue 各一份完全相同的分页逻辑
// (pageSize:20、showSizePicker、pageSizes:[20,50,100,200]、onPageChange/onPageSizeChange
// 重置页码并 reload),魔数 20/[20,50,100,200] 双份。合并为 composable,改默认改一处。

import { ref, type Ref } from 'vue'

export const DEFAULT_PAGE_SIZE = 20
export const DEFAULT_PAGE_SIZES: number[] = [20, 50, 100, 200]

export interface Pagination {
  page: number
  pageSize: number
  showSizePicker: boolean
  pageSizes: number[]
  itemCount: number
}

// usePagination 返回分页状态 + 处理器。
// onPageChange: 页码变化 → 更新 page 并 reload()
// onPageSizeChange: 页容量变化 → 重置到第 1 页并 reload()
export function usePagination(reload: () => Promise<void> | void) {
  const pagination = ref<Pagination>({
    page: 1,
    pageSize: DEFAULT_PAGE_SIZE,
    showSizePicker: true,
    pageSizes: DEFAULT_PAGE_SIZES,
    itemCount: 0,
  }) as Ref<Pagination>

  function onPageChange(page: number) {
    pagination.value.page = page
    void reload()
  }
  function onPageSizeChange(pageSize: number) {
    pagination.value.pageSize = pageSize
    pagination.value.page = 1
    void reload()
  }
  return { pagination, onPageChange, onPageSizeChange }
}
