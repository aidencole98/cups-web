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

纸张转换和预览使用同一尺寸，图片或文本转换后修改纸张/方向会使旧 PDF 缓存失效；转换字符串为 `custom:<宽mm>x<高mm>`。无边距图片生成 PDF 时去掉应用默认的 10mm 留边，保持照片比例；宽高比例不一致时仍可能留白，用户可先裁剪。已有 PDF 的内容白边不会自动裁掉。`/api/convert` 的 `borderless=true` 同步预览转换。

## 照片队列每次打印张数

创建 multipart 增加 `photos_per_run`，编辑 JSON 使用 `photosPerRun`；范围 1–100，旧请求和旧任务默认 1。数据库新增 `photos_per_run INTEGER NOT NULL DEFAULT 1`。张数与 `copies` 独立：每次 3 张、每张 2 份，会依次提交 3 个打印任务，每个任务 2 份。

- 非循环队列剩余不足设定张数时，提交剩余照片并结束。
- 循环队列到末尾后回到第一张，直到本次提交数量满足设定值；设定数大于队列长度时，同一批次可能重复照片。
- 每张成功提交后保存进度，失败停止当前批次；下次从失败照片继续。
- 自动和手动触发共用批次逻辑，整个批次只推进一个计划点；手动执行保留原暂停状态。
- 每张提交前持久化 `enabled=0, last_status=running`。每张成功保存进度时仍保持暂停，直到最后一个确定结果保存后才恢复计划；中断、落库失败、上传后断开、响应损坏或 HTTP 网关错误会保留暂停；明确连接失败或有效 IPP 拒绝响应才按失败继续下一次计划。核实打印机后由用户恢复。
- 最近执行保存最近 20 张照片的结果。`submittedCount` 和打印记录表示 CUPS 已接受，不代表物理出纸。
