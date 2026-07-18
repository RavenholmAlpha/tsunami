# XBoard TSUNAMI Overlay

此目录保存 TSUNAMI 对 XBoard 的源码覆盖层，不是完整的 XBoard 副本。

`overlay/` 下的路径与 XBoard 仓库根目录保持一致，包含节点类型、订阅、
节点控制面 API、凭据、Redis 设备 lease 和迁移所需的全部 14 个变更文件。
覆盖层基于 XBoard 提交 `8e4864b`（`master`）制作。

将该覆盖层应用到独立的 XBoard 工作副本时，从其仓库根目录复制
`overlay/` 的内容并保留相对路径，然后执行迁移：

```bash
php artisan migrate
```

不要向 XBoard 上游仓库提交此覆盖层。它只随 TSUNAMI 的私有集成分支维护。
