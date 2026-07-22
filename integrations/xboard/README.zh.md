# XBoard TSUNAMI Overlay

此目录保存 TSUNAMI 对 XBoard 的源码覆盖层，不是完整的 XBoard 副本。

`overlay/` 下的路径与 XBoard 仓库根目录保持一致，包含节点类型、订阅、
节点控制面 API、凭据、Redis 设备 lease、迁移和 TSUNAMI 管理端表单。
覆盖层基于 XBoard 提交 `8e4864b`（`master`）及
`xboard-admin-dist` 提交 `ef5f43d` 制作。

将该覆盖层应用到独立的 XBoard 工作副本时，从其仓库根目录复制
`overlay/` 的内容并保留相对路径，然后执行迁移。覆盖层已经带有补丁后的
管理端入口和静态资源，复制完成后 TSUNAMI 会直接出现在节点协议列表中：

```bash
php artisan migrate
node scripts/patch-tsunami-admin.mjs --check
```

`scripts/patch-tsunami-admin.mjs` 是管理端资源的可复现生成脚本。只有在重新
检出原始 `ef5f43d` 管理端静态包后才需要不带 `--check` 参数运行它。

不要向 XBoard 上游仓库提交此覆盖层。它只随 TSUNAMI 的私有集成分支维护。
