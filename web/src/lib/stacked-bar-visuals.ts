/**
 * 堆叠柱的**逐段视觉布局**（移植自 wb-switch 的同名模块，算法逐行对齐）。
 *
 * ## 解决什么问题
 *
 * recharts 按数值比例算每段的高度。于是当一个模型的消耗相对当天总量极小时
 * （真实例子：某天 459 积分里 `deepseek-v4-pro` 只占 1.28），它的段高不足 1px，
 * **肉眼看不见** —— 图例里有这个名字，柱子上却找不到它，读图的人会以为数据错了。
 *
 * 这里把「段的像素高度」重新分配一次：非零段保底 `minHeight`，缺口由**较胖的段
 * 按超出量等比例让出**。总量守恒，且小的那几段一定看得见。
 *
 * ## 为什么不用 `minPointSize`
 *
 * recharts 的 `minPointSize` 对**零值**同样生效 —— 结果是没有任何消耗的那天
 * 也会画出一小截柱子。那不是「看不清」，是**凭空多出一天有消耗**，
 * 属于正确性问题（本项目实测踩过）。所以最小高度必须自己按值判断：
 * 值为 0 直接不画，值为正才参与保底分配。
 */

export interface StackedSegmentVisualLayout {
  /** 调整后的段高（px）。 */
  height: number;
  /** 是否是这一柱中**实际最高**的非零段 —— 圆角只加在它上面。 */
  isTop: boolean;
  /** 调整后的段顶 y 坐标。 */
  y: number;
}

/**
 * 把各段的原始像素高度重新分配，保证每个非零段不低于 `minHeight`。
 *
 * 返回 null 表示无法分配（比例为 0 或全为 0），调用方回退到 recharts 的原始值。
 */
function allocateVisualHeights(
  values: number[],
  pixelsPerValue: number,
  minHeight: number,
): number[] | null {
  if (!Number.isFinite(pixelsPerValue) || pixelsPerValue <= 0) return null;
  const rawHeights = values.map((value) => Math.max(0, value * pixelsPerValue));
  const nonZero = rawHeights.filter((height) => height > 0);
  if (nonZero.length === 0) return null;

  const heights = [...rawHeights];
  const minTotal = nonZero.length * minHeight;
  const rawTotal = rawHeights.reduce((sum, height) => sum + height, 0);
  // 整柱太矮、连保底都放不下时：非零段一律取保底，不再按比例分。
  if (rawTotal < minTotal) {
    return heights.map((height) => (height > 0 ? minHeight : 0));
  }

  let deficit = 0;
  let donorExcess = 0;
  for (const height of heights) {
    if (height > 0 && height < minHeight) deficit += minHeight - height;
    if (height > minHeight) donorExcess += height - minHeight;
  }
  if (deficit <= 0 || donorExcess <= 0) return heights;

  // 缺口由「胖段超出保底的部分」按比例分摊，保证总量不变。
  return heights.map((height) => {
    if (height <= 0) return 0;
    if (height < minHeight) return minHeight;
    return height - (deficit * (height - minHeight)) / donorExcess;
  });
}

/**
 * 计算某一段的最终 y 与 height。
 *
 * `stackStart` 是这一段**下方**已累计的数值（recharts 在 2.x 会把它放进
 * shape 的 `value` 元组里；3.x 下由调用方用前面的段值求和得到）。
 * 有了它才能把坐标锚回整柱的基线 —— 只按单段自己的 y/height 调整，
 * 会让段与段之间出现缝隙或重叠。
 */
export function getStackedSegmentVisualLayout({
  values,
  segmentIndex,
  segmentHeight,
  segmentY,
  stackStart,
  minHeight = 5,
}: {
  values: number[];
  segmentIndex: number;
  segmentHeight: number;
  segmentY: number;
  stackStart: number;
  minHeight?: number;
}): StackedSegmentVisualLayout | null {
  const safeValues = values.map((value) => (Number.isFinite(value) && value > 0 ? value : 0));
  const segmentValue = safeValues[segmentIndex] ?? 0;
  if (segmentValue <= 0 || segmentHeight <= 0 || segmentIndex < 0) return null;

  const pixelsPerValue = segmentHeight / segmentValue;
  const heights = allocateVisualHeights(safeValues, pixelsPerValue, minHeight);
  if (!heights) return null;

  const baseline = segmentY + segmentHeight + Math.max(0, stackStart) * pixelsPerValue;
  const offsetBelow = heights.slice(0, segmentIndex).reduce((sum, height) => sum + height, 0);
  const height = heights[segmentIndex] ?? 0;
  // 最高的**非零**段才加圆角：整柱的顶可能是零值段，那就该由下面那段来收口。
  const topIndex = safeValues.reduce((result, value, index) => (value > 0 ? index : result), -1);

  return {
    height,
    isTop: segmentIndex === topIndex,
    y: baseline - offsetBelow - height,
  };
}
