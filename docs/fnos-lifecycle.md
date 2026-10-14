# fnOS 卸载目录规则

来源：[官方应用框架文档](https://developer.fnnas.com/docs/core-concepts/framework/)。
此前保存的官方页面及 [2026 年 4 月文档镜像](https://github.com/ckcoding/fnnas-docs/blob/a36552b83fdfd9fdc6c27e0a4d164b65bed32b44/docs/core-concepts/framework.md#应用卸载流程) 明确说明：

> 卸载应用时，系统会删除以下目录：`target`、`tmp`、`home`、`etc`，但会保留 `var` 和 `shares` 目录（保护用户数据）。
>
> 如果您希望提供完全删除选项，可以在 `wizard/uninstall` 向导中让用户选择是否保留数据。当用户选择不保留时，您可以在 `cmd/uninstall_callback` 中清理剩余的数据和目录。

Disk Bench 使用 `DATA_DIR="$TRIM_PKGVAR"`，历史保存到 `jobs/`，已属于平台默认保留的 `var`。

- 保留选项不复制、移动或改写历史，重装到原存储空间后直接读取原文件。
- 删除选项在卸载回调中清理 `jobs/`，同时处理使用过 0.1.10 时可能留下的额外副本。
- 不需要迁入 `data-share`，也不需要声明未经官方文档定义的“保留目录”字段。
- 测试应模拟平台保留 `var`、删除应用主体目录的实际顺序。
