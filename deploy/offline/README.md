# SkillForge 离线安装包

这个目录里放的是**离线一键安装**所需要的全部文件。客户机器可以完全没外网，把包拷过去跑一条命令就能用。

```
skillforge-offline-v0.3.0-linux-amd64/
├── install.sh                  ← 一键安装（装完自动自检）
├── uninstall.sh                ← 卸载（默认保留数据）
├── bin/skillforge              ← 编译好的服务端二进制，不依赖客户装 Go
├── fonts/gbsn00lp.ttf          ← 中文字体（PDF 生成靠它，见下面「为什么必须带字体」）
├── licenses/                   ← 字体许可证（分发字体必须随附）
├── skillforge.service.template ← systemd 服务模板
├── skillforge.env.example      ← 配置项说明书（真配置由 install.sh 生成）
├── VERSION / INSTALL.txt       ← 包里是什么、三步怎么装
└── README.md                   ← 本文件
```

---

## 一、三步装完

```bash
# 1. 拷到目标机器（用 scp / U 盘 / 内网共享都行，整个目录一起拷）
scp -r skillforge-offline-v0.3.0-linux-amd64 root@target:/root/

# 2. 在目标机器上执行
cd /root/skillforge-offline-v0.3.0-linux-amd64
sudo ./install.sh

# 3. 看输出的最后几行：访问地址 + 管理员密码（密码只显示一次）
```

装完打开 `http://<机器IP>:8092` 就能用了。

**整个过程不联网**：全程不 curl、不 wget、不 apt、不 pip。包里的东西就是全部依赖（服务端是静态编译的 Go 二进制，运行时除了 systemd 什么都不需要）。

常用参数：

| 目的 | 命令 |
| --- | --- |
| 换端口 | `sudo ./install.sh --port 9000` |
| 换安装目录 | `sudo ./install.sh --prefix /srv/skillforge` |
| 指定对外访问地址（影响下载链接） | `sudo ./install.sh --public-url https://sf.example.com` |
| 自己定管理员密码 | `sudo ./install.sh --admin-pass '你的密码'` |
| 重装/升级（同一条命令即可） | `sudo ./install.sh` |

**升级**就是拿新版本的包再跑一次 `install.sh`：它会保留已有的密钥、管理员账号和数据库，只替换程序和服务定义。所以升级前不用备份数据，但别带 `--purge`。

---

## 二、唯一的前置要求：systemd

安装脚本第一件事就是检查 `systemctl` 和 `systemd-run`，缺任何一个都**直接拒绝安装**，不会给你装出一个「能用但不能跑代码」的半残服务。

原因：这个服务最核心的能力之一，是让模型在**沙箱**里跑代码算数、生成文件、写中间结果。而沙箱是建立在 systemd 的降权机制上的（`systemd-run` 创建瞬时单元 → 降权到 `nobody` → 断网 → 只读文件系统 → 内存/CPU 限额）。没有 systemd 就没有沙箱，而**服务本身以 root 运行**，让 root 进程直接跑用户可控的代码等于把机器交出去。所以脚本宁可装不上，也不静默降级成裸跑。

支持 systemd 的发行版（Debian 9+、Ubuntu 16.04+、CentOS 7+、RHEL 8+、openSUSE、Arch……）都能装。

顺便一提：**服务以 root 运行是设计决定，不是疏忽**。创建 systemd 瞬时单元需要 root 或 polkit 授权，所以主进程必须是 root；真正跑用户代码的是它派生出去的那个降权子进程。为了缩小「主进程被攻破」的破坏面，服务单元里加了一组加固选项（`NoNewPrivileges`、`PrivateTmp`、`ProtectKernel*`、`RestrictSUIDSGID` 等）。

---

## 三、为什么包里必须自带字体

这不是锦上添花，是**数据正确性**问题。踩过的坑（内部编号 Bug G）：

PDF 生成库 gopdf 有个默认行为——**遇到字体里没有的字符，静默替换成空格**。于是只要字体没覆盖数字，生成的报价单里所有数量、单价、金额都会变成看不见的空白：文件照常产出、打开不报错、`%PDF` 魔数也对，只有数字没了。数据静默丢失，事后极难定位。

所以：

1. 包里自带 `gbsn00lp.ttf`（文鼎宋体，覆盖中文 + ASCII 数字 + 拉丁字母 + 半角标点），安装时装到 `/usr/local/share/fonts/skillforge/`；
2. 安装脚本把它**显式**写到配置 `SKILLFORGE_PDF_FONT_FILE`，不让程序在运行时去猜；
3. 装完的自检会**真的渲染一份 PDF 再回读文本**，逐字确认中文和数字都画出来了——而不是只检查「字体文件存在」。

**字体要求**：必须是 `.ttf`。gopdf 读不了 `.ttc`（字体集合，如 Noto CJK、wqy-zenhei）和 `.otf`（CFF 轮廓），会报 `Unrecognized file (font) format`。要换字体就换成 `.ttf`，改 `SKILLFORGE_PDF_FONT_FILE` 后 `systemctl restart skillforge`，再跑一次 `skillforge -selftest` 确认覆盖。

字体按 Arphic Public License 分发，许可证文本在 `licenses/`。**再分发时必须保留它**。

---

## 四、装完自检在查什么

`install.sh` 最后会跑一次 `/opt/skillforge/skillforge -selftest`（离线、不联网、不调模型），逐项给证据而不是给结论：

1. **版本信息** — 确认装的是哪个版本、哪个 commit 编的（CI 注入）。
2. **PDF 字体覆盖** — 真的生成一份 PDF，用纯 Go 解析回读文本，逐字比对中文与数字；缺字符会列出**缺哪几个**。它还会给出「嵌入字体」的硬证据，避免只看字体路径就误判通过。
3. **沙箱隔离** — 在沙箱里尝试联网（应当失败）、尝试读机密文件（应当读不到），证明降权与断网真的生效。这一项会读 `SKILLFORGE_ENV_FILE` 指向的文件作为「不该被读到的机密」样本。

任何一项不过，脚本不会假装成功：返回码非 0，并打印具体该改哪个文件、改完怎么验证。

---

## 五、卸载与数据

```bash
sudo ./uninstall.sh                # 卸载程序与服务，**保留数据目录**
sudo ./uninstall.sh --purge        # 连数据一起删（会二次确认）
```

默认保留数据是刻意的：客户卸载常常是为了重装或迁移，数据库里存着技能、模板、上传的文件，不该跟着消失。数据都在 `<安装前缀>/data/`（默认 `/opt/skillforge/data/`），**备份 = 拷这个目录**。

装的时候改过 `--prefix`，卸载时也要带同样的 `--prefix`，否则脚本会去默认路径找、然后告诉你「没有装过」。

---

## 六、常见问题

**装完打不开页面？**
```bash
systemctl status skillforge           # 服务活着吗
journalctl -u skillforge -n 50 --no-pager   # 看日志
ss -ltnp | grep 8092                  # 端口在听吗（换过端口就换成你的端口）
```
监听地址默认是 `:8092`（所有网卡）。只想本机访问就改配置里的 `SKILLFORGE_ADDR=127.0.0.1:8092`。

**防火墙 / 云安全组**：脚本不管防火墙。云主机要额外在安全组放行端口。

**PDF 里中文或数字空白**：跑 `sudo /opt/skillforge/skillforge -selftest`，看「PDF 字体」那一栏缺哪些字符，换一个覆盖它的 `.ttf` 并改 `SKILLFORGE_PDF_FONT_FILE`，重启服务。

**要改端口 / 加内网白名单 / 关掉代码执行**：改 `/opt/skillforge/skillforge.env`（里面每一项都有注释），然后 `systemctl restart skillforge`。全部可配置项见 `skillforge.env.example`。

**注意**：`skillforge.env` 里含随机生成的密钥（签名密钥、管理员密码、模型 API key），权限是 600。**不要提交进仓库、不要贴到聊天里、不要打包进任何要外发的文件**。

**模型怎么配**：装好后登录管理端网页配置，比改配置文件方便（网页配置优先）。离线环境接的模型需要在内网可达，否则智能体用不了。不用大模型时，模板填充、文档生成这些确定性功能照样可用。

---

## 七、从源码自己打这个包

```bash
# 1. 编译（Linux 目标，静态）
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags "-s -w -X github.com/lizhemin15/skillforge/internal/version.Version=v0.3.0" \
  -o /tmp/skillforge-linux-amd64 ./cmd/server

# 2. 打包（会自动挑字体、校验覆盖率、带上许可证）
scripts/build-offline-bundle.sh \
  --version v0.3.0 --arch amd64 --binary /tmp/skillforge-linux-amd64

# 输出：dist/skillforge-offline-v0.3.0-linux-amd64.tar.gz（+ .sha256）
```

打包脚本做三件容易被忽略的事：

- **架构核对**：用 `file` 读二进制真实架构跟 `--arch` 比对，防止把 arm64 的包装成 amd64（文件名是拼出来的，可能拼错）。
- **字体覆盖率实测**：直接解析字体 cmap 表，确认它真的含 `0-9`/拉丁/中文，覆盖不足就换下一个候选；全都不过就**报错退出**，绝不产出残包。
- **确定性打包**：固定时间戳、属主，按名字排序，`gzip -n`。同样的输入产出逐字节相同的 tar.gz，Release 里的 sha256 才有验证意义。

CI（`.github/workflows/release.yml`）会在打 tag 时自动为 `linux/amd64` 和 `linux/arm64` 各产出一个离线包并附到 Release，同时保留原来那种「裸二进制」产物。
