# 打印机选项与照片批次（issue #122）

普通打印、重新打印和定时任务共用驱动提供的纸张大小、介质类型、打印质量。CUPS 队列优先通过现有安全 HTTP/Unix Socket 连接读取 `/printers/<队列>.ppd`；没有 PPD 或某一类选项缺失时，查询标准 IPP 能力。读取失败会在界面显示原因，可点击「刷新选项」重试。

## 能力接口

`GET /api/printer-options?uri=<队列URI>` 使用已有登录/API Key 鉴权。每个目标缓存 60 秒，缓存最多保存 64 个目标；`force=1` 绕过缓存，失败不覆盖已有结果。

响应包含：

- `paperSizes`、`mediaTypes`、`qualities`：选项数组。
- `defaults`：`pageSize`、`mediaType`、`quality` 的驱动默认选择（可缺省）。
- `source`：`ppd` 或 `ipp`；PPD 缺失类别仍可补充标准 IPP 选项。

每个选项携带 `attribute`、`value`、`label`。纸张另有 `widthMM`、`heightMM`、`borderless`；标准 IPP 的 `media-col` 选择保存四边距（1/100 mm，顺序为下、左、右、上）。PPD 中如存在 `zh_CN` 或 `zh` 翻译（优先 `zh_CN`）则使用其标签，否则保留驱动标签。

PPD 质量控件优先选择 `cupsPrintQuality`、`PrintQuality`、`Quality`，其次识别标题为 Print Quality 的厂商控件（例如 Epson 的 `Ink`），最后使用 `Resolution`。只展示纸张、介质、质量三类，其他厂商调整项不开放。PPD 只作为声明解析，不执行其中的代码。

PPD 的 ImageableArea 覆盖整张纸时识别为无边距，包括可打印区域向纸外扩展的负边距规格。

标准 IPP 显式请求 `media-col-database`，有完整尺寸和四边距时生成普通／无边距规格；否则解析 `media-supported` 中的 PWG 尺寸。质量使用 IPP 枚举 3/4/5。最终纸张、介质、质量组合的驱动限制由 CUPS 判断。

## 提交与参数快照

`POST /api/print` 和 `POST /api/scheduled-prints` 的 multipart 增加 `driver_options` JSON 字符串：

```json
{
  "pageSize": {"attribute": "PageSize", "value": "实际驱动取值"},
  "mediaType": {"attribute": "MediaType", "value": "实际驱动取值"},
  "quality": {"attribute": "Ink", "value": "实际驱动取值"}
}
```

应从能力接口复制所选项；字段可以省略，打印质量不选时使用打印机默认。`POST /api/print-records/{id}/reprint` 和 `PUT /api/scheduled-prints/{id}` 在 JSON 中使用 `driverOptions` 对象。服务端按目标队列重新匹配 attribute/value，并用上报的尺寸与标签覆盖客户端元数据；不支持的选择返回错误，不静默换成另一规格。

选择驱动纸张／介质后，不再发送对应的传统 `media`／`media-type` 映射，避免冲突。PPD 选项保留原名称和选择值；IPP 质量按 enum 编码，无边距 `media-col` 按 collection 编码。IPP 的成功状态包含 0x0001（替代属性）和 0x0002（冲突属性已调整），这些响应仍表示任务被接受，推进照片进度。

`print_jobs`、`scheduled_prints` 幂等增加 `driver_options_json TEXT NOT NULL DEFAULT '{}'`，保存选项快照供重打与编辑预填。旧客户端仍可使用原有 `paper_size`／`paper_type` 参数。定时任务未改变队列和选择时，编辑或暂停复用数据库中的可信快照，因此打印机离线也能暂停；实际执行仍验证选项是否存在。

纸张转换和预览使用同一尺寸，图片或文本转换后修改纸张/方向会使旧 PDF 缓存失效；转换字符串为 `custom:<宽mm>x<高mm>`。无边距图片生成 PDF 时去掉应用默认的 10mm 留边；普通规格仍保留该应用留边，不按驱动的硬件边距重新计算。`/api/convert` 的 `borderless=true` 同步预览转换。

### 照片适应与填充

`/api/convert` 支持可选的 `print_scaling` 字段，单图 `file` 和多图 `files` 均生效，缺省保持原有完整显示布局。普通打印、批量转换、原图重打和照片定时队列复用相同排版：

- `fit`（适应纸张）：等比缩放、完整显示照片，比例不一致时允许留白。
- `fill`（填充纸张）：等比放大、居中裁切，铺满内容区。无边距规格铺满整页；普通规格铺满扣除四边各 10mm 应用留边后的区域。
- 多图合一先应用逐图旋转，再按每个格子的比例分别适应或填充，保留格子间的 4mm 间隔。PDF 裁剪路径限制照片的可见区域，不额外重编码图片。

图片转换后切换填充模式会使旧 PDF 失效；过期转换响应不会覆盖当前选择。原图预览同步适应/填充与应用留边，多图首张预览只是示意，合并后的 PDF 展示完整排版。其他缩放模式仍沿用原有图片布局与 CUPS 处理，自定义百分比继续使用现有缩放管线。

已有 PDF 的内容白边不会自动裁掉。重新打印时，有原图的记录可重新适应/填充；只有 PDF 的记录按已有页面打印，无法按原图重新排版，需要重新上传原图才能改变照片布局。

## 照片拍摄日期（issue #124）

普通照片打印的「高级选项」与照片定时任务提供「打印拍摄日期」开关，默认关闭。开启后，在每张照片最终可见区域的右下角绘制橙色日期及轻微阴影，格式为 `2026年10月3日`。适应布局贴照片边缘，填充布局贴裁切后的内容区边缘；多图排版在每张照片内分别绘制。点击「转换为 PDF」可查看结果，修改开关会使旧 PDF 缓存失效。

只使用 JPEG EXIF 或 PNG `eXIf` 中的 `DateTimeOriginal`（拍摄时间），保留其年月日，不做时区换算。缺失、损坏或日期无效时正常打印该照片并跳过日期，不使用文件时间或执行时间替代。GIF 通常没有 EXIF；HEIC 经现有浏览器转码后若未保留拍摄日期，也会跳过。本功能不改变既有 EXIF 方向处理。

接口沿用现有路由：

- `POST /api/convert`、`POST /api/print`、`POST /api/scheduled-prints` 的 multipart 使用 `photo_date_watermark=true`。
- `PUT /api/scheduled-prints/{id}`、`POST /api/print-records/{id}/reprint` 的 JSON 使用 `photoDateWatermark: true`；列表响应也包含此布尔字段。
- `/api/convert` 可额外提交 `photo_dates` JSON 字符串数组，如 `["2026-10-03", ""]`，按 `file`/`files` 顺序逐图对应，空串表示没有有效拍摄日期。数组必须与文件数量相同；日期严格校验为有效的 `YYYY-MM-DD`。不提供数组时由服务端读取原图。浏览器在压缩前读取 EXIF，并通过该字段传递日期，因此压缩不会丢失日期；此字段只提供显示内容，不参与文件定位。

`print_jobs` 和 `scheduled_prints` 幂等增加 `photo_date_watermark INTEGER NOT NULL DEFAULT 0`，保存打印、重打和定时任务的设置。旧记录保持关闭，旧客户端编辑定时任务时省略该字段会保留原值。定时任务执行时逐张读取持久化原图，日期绘制不改变提交进度或调度规则。

有原图的记录可在重打时切换日期；已经生成 PDF 的记录保留页面上的日期，重打不重复叠加，也不能移除已绘制的日期。其他文档类型不应用该开关。日期与既有自定义文字水印可同时使用。

## 照片队列每次打印张数

创建 multipart 增加 `photos_per_run`，编辑 JSON 使用 `photosPerRun`；范围 1–100，旧请求和旧任务默认 1。数据库新增 `photos_per_run INTEGER NOT NULL DEFAULT 1`。张数与 `copies` 独立：每次 3 张、每张 2 份，会依次提交 3 个打印任务，每个任务 2 份。

- 非循环队列剩余不足设定张数时，提交剩余照片并结束。
- 循环队列到末尾后回到第一张，直到本次提交数量满足设定值；设定数大于队列长度时，同一批次可能重复照片。
- 每张成功提交后保存进度，失败停止当前批次；下次从失败照片继续。
- 自动和手动触发共用批次逻辑，整个批次只推进一个计划点；手动执行保留原暂停状态。
- 每张提交前持久化 `enabled=0, last_status=running`。每张成功保存进度时仍保持暂停，直到最后一个确定结果保存后才恢复计划；中断、落库失败、上传后断开、响应损坏或 HTTP 网关错误会保留暂停；明确连接失败或有效 IPP 拒绝响应才按失败继续下一次计划。核实打印机后由用户恢复。
- 最近执行保存最近 20 张照片的结果。`submittedCount` 和打印记录表示 CUPS 已接受，不代表物理出纸。

## 照片队列自动方向

新建照片定时队列默认选择「自动」方向，也可以手动选择纵向或横向；已有任务保留原有方向。单文件定时任务不提供自动方向，从照片队列切换到单文件时会将自动方向改为纵向。

创建 multipart 和编辑 JSON 沿用 `orientation` 字段，照片队列可以传 `auto`，单文件任务传此值会返回 400。复用 `scheduled_prints.orientation` 存储，无需新增列。

自动执行和「打印下一批」都会逐张读取原图的实际像素尺寸：宽大于高使用横向，否则使用纵向（包括正方形）。每张生成的 PDF、IPP 参数和打印记录使用同一实际方向，队列仍保留 `auto`。图片读取或转换失败时停止批次，保留失败照片的位置。

本次按实际像素宽高判断，用户保存旋转后的照片会按新尺寸判断；不读取 JPEG EXIF 方向，也不新增旋转操作。
