# NextBot

NextBot 面向自托管的多 Agent 编排与跨端控制。首个使用场景是在电脑上派发仓库任务，从手机处理授权、查看证据并验收当前版本。仓库目前处于产品与工程设计阶段。

## 工程开发文档

| 文档 | 读完可以决定什么 |
| --- | --- |
| [架构与技术选型](docs/engineering/01-architecture.md) | 如何在一台主机上部署控制服务与 Runner，以及哪些能力需要先做实测 |
| [接口与数据交换协议](docs/engineering/02-api-protocol.md) | 桌面、手机、控制服务与执行节点交换哪些请求、事件和回执 |
| [持久化与故障恢复](docs/engineering/03-data-recovery.md) | 任务、产物、审批存在哪里，断线、重启和重放时如何核对结果 |
| [P1 开发任务与验收计划](docs/engineering/04-p1-delivery.md) | 各项工作依赖什么，哪些证据齐全后可以进入内部试用 |

## 产品依据

- [产品需求文档](docs/product/01-prd.md) 定义用户、产品范围、竞品取舍和发布条件。
- [产品体验与操作说明](docs/product/02-product-guide.md) 描述一次任务从桌面派发到手机验收的过程。
- [功能规格与验收](docs/product/03-functional-spec.md) 规定任务状态、权限边界及各阶段验收用例。

P1 验证单用户、单节点、Codex 执行与人工验收。P2 在同一节点增加独立只读复核，作为首个可用发布版本。P3 再验证多节点委派与非代码产物。
