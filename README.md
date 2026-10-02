# ShellSmith

这是一个面向命令行工具的命令行工具框架与终端应用平台。长期目标是提供命令树与别名、参数解析与校验、帮助与补全生成、配置分层、交互提示、表格与进度与彩色输出、结构化输出和插件扩展，把 CLI 开发沉淀为可复用框架。

仓库采用 Go，当前冻结基线只提供进程健康检查。后续能力必须通过独立题目逐步实现；每个题目都应定义可观察的公共行为、兼容边界和失败语义，不得依赖未公开内部 API。

## 启动

```bash
go run ./cmd/shellsmith
```

服务默认监听 `127.0.0.1:8080`。可通过 `SHELLSMITH_ADDR` 修改监听地址。`GET /healthz` 返回 JSON 健康状态。

## 验证

```bash
go test ./...
```

当前基线刻意不包含参数解析与补全生成的实现，以便后续任务从已冻结事实出发独立设计并验证这些能力。

## 命令树（`command` 包）

`github.com/f5dt1artum/shellsmith/command` 是可供任意 Go 程序复用的公开包，提供根命令、任意深度子命令与别名的声明与分发，不依赖 HTTP 服务。

- `command.New(name, description, aliases, handler)` 创建节点；handler 可为 `nil`。
- `(*Command).Add(child)` 挂接子节点并返回接收者，成功时保留注册顺序。
- `(*Command).Execute(ctx, args, stdout, stderr)` 逐段匹配并调用处理函数。

处理函数签名为：

```go
type Handler func(ctx context.Context, path []string, args []string, stdout, stderr io.Writer) error
```

匹配严格区分大小写、只接受完整主名称或别名，不做前缀猜测。别名命中后 `path` 仍报告规范主名称路径，剩余参数保持原顺序与内容整体交给叶子节点的处理函数；处理函数返回的错误由 `Execute` 原样返回。框架不生成帮助、补全或日志，也不自行写入两个输出流。

```go
root, _ := command.New("tool", "示例工具", nil, nil)
build, _ := command.New("build", "构建", []string{"b"}, func(ctx context.Context, path, args []string, out, errOut io.Writer) error {
    fmt.Fprintf(out, "path=%v args=%v\n", path, args)
    return nil
})
_, _ = root.Add(build)

// 以下两种调用等价，path 均为 ["tool", "build"]
_ = root.Execute(ctx, []string{"build", "pkg"}, os.Stdout, os.Stderr)
_ = root.Execute(ctx, []string{"b", "pkg"}, os.Stdout, os.Stderr)
```

注册失败不会改变树，错误均可用 `errors.Is` 识别：

| 错误 | 触发条件 |
| --- | --- |
| `ErrInvalidName` | 主名称或别名为空、含 `A-Za-z0-9_-` 以外字符或以连字符开头（节点内重复别名同属冲突，见下） |
| `ErrInvalidCommand` | `Add` 收到 `nil` |
| `ErrNameConflict` | 同一父节点下任何主名称或别名相互重复，包括节点自身主名称与别名、别名与别名重复 |
| `ErrAlreadyAttached` | 节点已挂接到其他（或同一）父节点后再次挂接 |

执行期错误：

| 错误 | 触发条件 |
| --- | --- |
| `ErrUnknownCommand` | 下一参数不匹配任何子命令；实际返回的是携带已解析规范路径 `Path` 与未识别参数 `Arg` 的 `*LookupError`，不调用处理函数、不写输出 |
| `ErrCommandRequired` | 参数耗尽而到达节点没有处理函数 |

有处理函数的节点即使没有剩余参数也会被调用。`Execute` 在分发前发现 `ctx` 已取消或超时，会直接返回 `context.Canceled` 或 `context.DeadlineExceeded`。

