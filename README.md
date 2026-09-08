# Go-TCP-Proxy ✨

## Made with love ❤️



 一个用 Go 写的小小 TCP 代理：把本地端口收到的每一条连接，都乖乖地经由 Socks5 代理送往远程目标。



## 🧐 这是什么？



有些程序天生"不懂"代理——它们只会老老实实地连一个 IP 和端口，配置里压根没有"Socks5"这一栏。Go-TCP-Proxy 就是为它们准备的一根"延长线"：

```
你的程序  ──▶  127.0.0.1:10808 (Go-TCP-Proxy)  ──▶  Socks5 代理  ──▶  远程目标
```
你的程序只管连本地端口，剩下的绕路、握手、双向搬运数据，统统交给它。

## 🌟 Go-TCP-Proxy 的亮点

### 🔌 一根线的事

一个本地端口，对应一个远程目标，中间走一个 Socks5 代理。没有花里胡哨的规则引擎，也不需要写一大篇配置——它只做一件事，并且把这件事做好。

### 🧭 交互式配置

启动后它会一句一句地问你：本地监听哪里？远程目标是谁？Socks5 代理在哪儿？每个问题都带着默认值，直接按回车就好。第一次用也不会迷路～

### 💾 记住你的选择

每次确认过的配置都会自动保存到系统的用户配置目录里。下次启动时，一路回车就能沿用上次的设置，再也不用翻聊天记录找端口号啦。

### 🌊 全双工转发

每条连接都会开两个 goroutine 分别负责"客户端 → 远程"和"远程 → 客户端"。任何一方发送完毕（TCP 半关闭）时，只会把"我说完了"这个信号原样传给另一方，另一方仍然可以把剩下的响应发回来——像 HTTP/1.0、`nc -N`、SSH 管道传输这类依赖半关闭语义的协议也能正常工作。两边都结束后连接才真正关闭，不留僵尸连接。

### 🛑 优雅退出

按下 `Ctrl + C` 时，它会先停止接受新连接，然后等待正在传输的连接结束，最后才安静离场。正在下载的东西不会被无情掐断。

等待有上限（默认 5 秒，可用 `-shutdown-timeout` 调整）；超时或者你再按一次 `Ctrl + C`，剩余连接会被立即关闭——不会因为某个空闲的长连接而永远退不出去。

### 📦 单文件、零依赖、跨平台

`CGO_ENABLED=0` 静态编译，一个可执行文件拷到哪里都能跑。每次推送到主分支，GitHub Actions 都会自动构建以下平台并发布到 [Nightly Release](https://github.com/lingyicute/Go-TCP-Proxy/releases/tag/nightly)：

| 系统 | 架构 |
| --- | --- |
| Linux | amd64 / arm64 |
| Windows | amd64 / arm64 |
| macOS | amd64 (Intel) / arm64 (Apple Silicon) |

## 🚀 快速开始

### 方式一：下载现成的二进制

前往 [Nightly Release](https://github.com/lingyicute/Go-TCP-Proxy/releases/tag/nightly) 页面，下载对应你系统的 `tcp-proxy-<os>-<arch>`（同页的 `SHA256SUMS.txt` 可用于校验）。

### 方式二：`go install`

```bash
go install github.com/lingyicute/Go-TCP-Proxy/cmd/tcp-proxy@latest
```

### 方式三：从源码构建

需要 Go 1.25 或更高版本。

```bash
git clone https://github.com/lingyicute/Go-TCP-Proxy.git
cd Go-TCP-Proxy
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o tcp-proxy ./cmd/tcp-proxy
```

### 运行

```bash
./tcp-proxy          # Linux / macOS

tcp-proxy.exe        # Windows
```



然后你会看到这样的对话：



```
 -----       Socks5 TCP 代理工具 By 梨       -----

 请根据提示输入配置信息，直接按回车将使用上次保存的值。

 请输入本地监听地址和端口 [127.0.0.1:10808]:

 请输入远程目标服务地址和端口 [example.com:80]:

 请输入 Socks5 代理地址和端口 [127.0.0.1:1080]:
```
回答完三个问题，服务就启动啦。之后让你的程序去连 `127.0.0.1:10808` 就好。

### 非交互运行（脚本 / systemd / Docker）

给出任意一个地址参数或 `-y`，就不会再提问：

```bash
# 只改远程目标，其余沿用上次保存的配置
tcp-proxy -r your-server:22

# 全部显式指定，且本次不写回配置文件
tcp-proxy -l 127.0.0.1:10808 -r db.internal:5432 -s 127.0.0.1:1080 --no-save

# 直接使用上次保存的配置
tcp-proxy -y

# 多开实例时给每个实例单独的配置文件
tcp-proxy -c ~/.config/go-proxy-tunnel/ssh.json -r your-server:22
```

完整参数见 `tcp-proxy -h`。

## ⚙️ 配置说明

| 配置项 | JSON 字段 | 默认值 | 说明 |
| --- | --- | --- | --- |
| 本地监听地址 | `local_addr` | `127.0.0.1:10808` | Go-TCP-Proxy 在本机监听的地址和端口 |
| 远程目标地址 | `remote_addr` | `example.com:80` | 你真正想连接的服务，支持域名或 IP |
| Socks5 代理地址 | `socks_addr` | `127.0.0.1:1080` | 中间经过的 Socks5 代理（暂不支持用户名密码认证） |

命令行参数的优先级高于配置文件。只有在监听成功之后配置才会被写回文件——输错的地址不会变成下次的默认值。

配置文件会保存为 `config.json`（权限 `0600`），位置因系统而异，也可以用 `-c` 指定：

| 系统 | 路径 |
| --- | --- |
| Linux | `~/.config/go-proxy-tunnel/config.json` |
| macOS | `~/Library/Application Support/go-proxy-tunnel/config.json` |
| Windows | `%AppData%\go-proxy-tunnel\config.json` |

文件内容长这样，你也可以直接手动编辑（缺少的字段会自动使用默认值）：

```json
{
  "local_addr": "127.0.0.1:10808",
  "remote_addr": "example.com:80",
  "socks_addr": "127.0.0.1:1080"
}
```

## 🧪 它能拿来做什么？

* **让不支持代理的客户端走代理** —— 数据库客户端、游戏、老旧的桌面软件……只要能填 IP 和端口，就能借它绕路。
* **通过 Socks5 使用 SSH** —— 把 `remote_addr` 设成 `your-server:22`，然后 `ssh -p 10808 user@127.0.0.1`。
* **给远程服务做本地"别名"** —— 把某个远程端口映射到本地固定端口，方便调试。

> [!tip]
> 小提示：如果希望局域网内其它设备也能使用，可以把本地监听地址改为 `0.0.0.0:10808`。不过请务必确认你的网络环境是可信的哦。

## 🤗 常见问题

### Q：它和 Socks5 代理本身有什么区别？

Socks5 代理需要客户端"会说 Socks5 协议"才能使用；Go-TCP-Proxy 则是替那些不会说的客户端代劳，对客户端来说它就是一个普通的 TCP 端口。

### Q：可以同时转发多个远程目标吗？

一个实例只对应一个远程目标。想转发多个目标，就多开几个实例，各自监听不同的本地端口即可。建议用 `-c` 给每个实例指定单独的配置文件，或者直接用 `-l/-r/-s --no-save` 传参，避免互相覆盖。

### Q：Socks5 代理需要用户名和密码怎么办？

目前版本使用的是无认证的 Socks5 拨号器，暂时不支持账号密码认证。欢迎 Pull Request 呀！

### Q：启动时提示"无法监听本地端口"？

多半是端口被占用了，换一个端口试试；在 Linux / macOS 上监听 1024 以下的端口还需要管理员权限。

### Q：提示"通过代理连接到 … 失败"？

请先确认 Socks5 代理本身在运行且地址填写正确，再确认代理能够访问到远程目标。

### Q：日志里会出现哪些错误信息？

连接正常关闭产生的错误（`use of closed network connection`、`connection reset by peer`、`broken pipe` 等）已经被过滤掉，不会打印。如果看到其它错误信息，通常说明对端异常断开或代理不可达，一般不影响后续新连接。

连接代理和 Socks5 握手默认有 10 秒超时（`-dial-timeout` 可调），代理无响应时不会让连接永远挂着。

### Q：提示"当前监听在非回环地址"是什么意思？

你把本地监听地址设成了 `0.0.0.0:...` 或 `:...` 之类的地址，同一网络中的其它设备也能连上这个端口并借此访问远程目标。确认这是你想要的，并且网络环境可信。

### Q：为什么配置目录叫 `go-proxy-tunnel` 而不是 `Go-TCP-Proxy`？

历史原因——早期版本的模块名就叫 `go-proxy-tunnel`，配置目录沿用了它。改名会让老用户的配置"失踪"，所以就这么留着啦。

## 🛠️ 开发

```
cmd/tcp-proxy/     命令行入口：参数解析、交互式提问、信号处理
internal/tunnel/   核心转发：Accept 循环、Socks5 拨号、半关闭传播、优雅停机
internal/config/   配置的默认值、加载、校验、原子保存
```

```bash
go test -race ./...     # 单元测试（含内置的 Socks5 测试服务器）
go vet ./...
gofmt -l .
```

CI 会在每次推送时运行 gofmt / go vet / staticcheck / govulncheck / go test，全部通过后交叉编译六个平台，并把主分支的产物发布到 Nightly Release。

## 🗂️ License

Go-TCP-Proxy is released under the GNU Affero General Public License v3.0 (AGPLv3).

Copyright (C) 2025-2026 lingyicute.

This program is free software: you can redistribute it and/or modify it under the terms of the GNU Affero General Public License as published by the Free Software Foundation, either version 3 of the License, or (at your option) any later version.

This program is distributed in the hope that it will be useful, but WITHOUT ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License along with this program. If not, see <https://www.gnu.org/licenses>.
