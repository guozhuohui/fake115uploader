# fake115uploader 增加 ED2K 链接生成功能：分析与实现方案

> 目标：在现有上传流程成功后，为每个文件生成一个 **115 可用的 ED2K 链接**。
> 本文先分析现有代码，再汇总联网调研结果，最后给出可直接落地的改造方案、测试方案和风险点。
>
> **实现状态**：核心代码已按本方案落地，涉及 `ed2k.go`、`hash.go`、`fast.go`、`multipart.go`、`oss.go`、`main.go`，并新增 `ed2k_test.go`。已使用 Go 1.26.10 执行 `gofmt`、`go vet .`、`go build ./...` 和 `go test .`，主包全部通过；`go test ./...` 中 `cipher.TestGenKey` 为仓库既有失败（本次未改动 `cipher`，上游测试预期相同）。本文中的代码片段以设计说明为主，最终以仓库源码为准。

---

## 0. 结论摘要

1. **技术上可行**。ED2K 链接只由「文件名 + 文件字节数 + ED2K 哈希」组成，哈希可以在本地对文件流式计算，不需要 115 提供额外接口。
2. **115 端确实是按哈希找文件的**。115 添加 ED2K 离线任务时，会优先在服务端资源中按哈希查找；命中时直接转存（“秒离”），并使用 115 侧已存的文件名，**链接里的文件名不参与匹配**。相关证据见 [115 社区 T787020](https://115.com/115/T787020.html) 和 [fake115uploader issue #48](https://github.com/orzogc/fake115uploader/issues/48) 中“秒链弄成 ed2k 也一样可以秒离”的讨论。
3. **上传到 115 不等于一定“秒离”**。如果 115 服务端没有该哈希对应的文件，115 会走真实下载，能否成功取决于 ED2K 供源，而不是本工具能控制的。因此本功能应定位为“生成正确的文件身份标识”，并在文档中说明这一前提。
4. **唯一需要实测的兼容性点是 ED2K 的 Blue/Red 变体**：当文件大小恰好是 `9,728,000` 字节的整数倍时，两种实现会得到不同哈希。建议默认在边界情况下同时输出两个链接，并用 115 实测确定主链接；非边界文件两种算法结果相同，不存在该问题。
5. **实现上应把 ED2K 计算合并进现有 SHA1 读取过程**，只多一次 MD4 计算、不额外读盘；用 `-ed2k` 开关控制，默认关闭，避免给不需要该功能的用户增加 CPU 开销。
6. **结果输出保持向后兼容**：`result.json` 的 `success` 仍是字符串数组，新增独立的 `ed2k` 数组；控制台新增独立段落，不破坏现有脚本解析。

---

## 1. 现有项目分析

### 1.1 代码结构

| 文件 | 职责 |
| --- | --- |
| `main.go` | 参数/配置、文件收集、三种上传模式调度、结果输出 |
| `fast.go` | 秒传（SHA1 `initupload`）、`fastToken` 结构 |
| `oss.go` | 普通 OSS 上传、115 文件列表校验、HTTP 辅助函数 |
| `multipart.go` | OSS 分片/断点续传、`saveProgress` 存档 |
| `hash.go` | 文件 SHA1、指定范围 SHA1 |
| `cipher/cipher.go` | 115 的 ECDH/RSA 加密逻辑 |

### 1.2 当前上传流程

`main.go` 的 `uploadFile()`（`main.go:706-767`）按参数分三条路径：

- `-f`：`fastUploadFile()` → 只走秒传；
- `-u`：先 `fastUploadFile()`，失败后 `ossUploadFile(token, path)`；
- `-m`：先 `fastUploadFile()`，失败后 `multipartUploadFile(token, path, nil)`；若存在存档则 `resumeUpload(path)`。

三条路径的共同点：

1. 最终都会拿到 `fastToken`（秒传成功时由响应解析，秒传失败时也携带 OSS 上传所需的 `bucket/object/callback`）。
2. `fastToken.SHA1` 在 `fast.go:183-185` 被设置为文件的整体 SHA1。
3. 成功时只把 `file.Path` 追加到 `result.Success`，没有保存每个文件的哈希、大小、115 文件名等元数据。

### 1.3 现有哈希逻辑

`hash.go:37-59` 的 `hashSHA1(f)`：

- 先读文件前 128 KiB 算 `blockHash`（目前调用方未使用）；
- `Seek(0)` 后对整个文件算 SHA1；
- 返回 `(blockHash, totalHash)`。

`fast.go:129-170` 的 `uploadFileSHA1()` 调用它取得整体 SHA1，再调用 `uploadSHA1()`；如果 115 返回 `status=7/statuscode=701`，再用 `hashFileRange()` 对指定区间算 SHA1 并重试。

**结论：文件在秒传阶段已经被完整读了一遍，这是合并计算 ED2K 的最佳位置。**

### 1.4 现有结果与配置

- `uploadConfig`（`main.go:78-86`）：cookies、cid、resultDir、httpRetry、代理、partsNum。
- `resultData`（`main.go:95-99`）：`Success []string`、`Failed []failedFile`、`Saved []string`。
- `exitPrint()`（`main.go:196-232`）：把结果写入 `时间 result.json`，并打印成功/失败/存档文件。
- `fastToken`（`fast.go:37-49`）：已有 `SHA1` 字段（无 JSON tag），分片存档 `saveProgress` 会整体序列化它。

### 1.5 改造需要特别注意的现有问题

1. **`-e` 会提前删除源文件**：秒传成功后 `fast.go:196-199` 立即删除本地文件；如果 ED2K 哈希等到上传后再算，就没有文件可读了。必须在删除前完成哈希（合并进 SHA1 计算即可）。
2. **断点续传存档缺少哈希**：`saveProgress` 只存 `FastToken/Chunks/Imur/Parts`，旧存档没有 ED2K 字段；恢复上传时需要兼容处理（重算或跳过）。
3. **OSS/分片上传的校验用了 `config.CID` 而不是 `file.ParentID`**（`oss.go:200`、`multipart.go:252`）：递归上传子文件夹时可能校验错目录。ED2K 功能本身不依赖这个校验，但顺手修正会更稳。
4. **空文件会触发 panic**：`hashSHA1` 里 `n, err := f.Read(block); checkErr(err)` 对空文件会拿到 `io.EOF` 并 panic。新的流式哈希实现应顺手处理 `io.EOF`。
5. **`blockHash` 目前未被使用**：可以在新实现中保留或清理，不影响 ED2K。

---

## 2. 联网调研结果

### 2.1 ED2K 哈希算法

ED2K 使用 **MD4**，但并不是对整个文件直接做一次 MD4：

- 文件按 **9,728,000 字节（9,500 KiB）** 分块；
- 每块单独计算 MD4（16 字节）；
- 多块文件把所有块的 MD4 按顺序拼接，再对拼接结果做一次 MD4，得到最终哈希；
- 文件小于一块时，直接返回该块的 MD4；
- 空文件的哈希是 `MD4("")`。

**Blue / Red 变体**：

- **Red**：原始/eMule 兼容实现，当文件大小恰好是 9,728,000 的整数倍时，会在块哈希列表末尾再追加一个「空块」的 MD4，再算根哈希。
- **Blue**：修正后的实现，不追加空块。
- 两者只在「文件大小是 9,728,000 整数倍」时不同；其他文件完全一致。

权威实现与说明：

- [RHash 的 ed2k.c](https://github.com/rhash/RHash/blob/master/librhash/ed2k.c)：注释明确写的是 **eMule-compatible**，整数倍时会 `final` 一个空块；`not_emule` 分支才是不追加空块的版本。
- [Kimundi/ed2k](https://github.com/Kimundi/ed2k)：同时实现 Red/Blue，并给出整数倍测试向量。
- [zorchenhimer/go-ed2k](https://github.com/zorchenhimer/go-ed2k)：`SumBlue()` / `SumRed()` 两个方法。
- [go.felesatra.moe/hash/ed2k](https://pkg.go.dev/go.felesatra.moe/hash/ed2k)：实现 `hash.Hash`，默认 Blue，另提供 `AltHash` 表示 Red。
- [chocko124/ed2k-link-generator-toolkit](https://github.com/chocko124/ed2k-link-generator-toolkit)：Python 流式实现，默认 Blue，`--mode red` 可切换，README 明确提到 115/PikPak 场景。
- [ed2ktool.com](https://www.ed2ktool.com/)：浏览器本地计算的在线生成器。

**测试向量**（来自 RHash/Kimundi/chocko 的公开测试）：

| 输入 | 期望哈希 |
| --- | --- |
| 空文件 | `31D6CFE0D16AE931B73C59D7E0C089C0` |
| `"a"` | `BDE52CB31DE33E46245E05FBDBD6FB24` |
| `"abc"` | `A448017AAF21D8525FC10AE87AA6729D` |
| `"hello world"` | `AA010FBC1D14C795D86EF98C95479D17` |
| 9,728,000 个 `0x55`（Blue） | `4127A47867B6110F0F86F2D9845FB374` |
| 9,728,000 个 `0x55`（Red） | `49E80F377B7E4E706DBD3ECC89F39306` |

### 2.2 ED2K 链接格式

标准格式：

```text
ed2k://|file|<文件名>|<字节数>|<32 位十六进制哈希>|/
```

实际 115 链接中文件名一般直接写原始 UTF-8：中文、空格、`[` `]`、全角符号等都保持原样，**不需要做 URL 编码**。部分网站会使用百分号编码，但这不是 ED2K 链接的必需格式，115 也不要求。

示例：

```text
ed2k://|file|中文 hello ?.txt|42|AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA|/
```

注意：`|` 是 ED2K 链接的字段分隔符，文件名本身不能包含 `|`（Windows 文件名也不允许该字符）。

补充：

- 哈希大小写不敏感，115 的示例多为大写，建议统一大写。
- 可选的 `|h=<AICH 哈希>|/` 是 eMule 的 AICH 标签，115 不要求；没有它也能正常添加（T787020 中的示例就没有 `h=`）。本功能不计算 AICH。
- 链接里的文件名对 115 的匹配 **不重要**：T787020 中用户发现 115 离线完成后使用的是 115 服务端已有的文件名（带广告），而不是链接里的文件名；回帖明确说“他不是真下是服务器内找文件，所以自定义不了名字”。因此我们主要保证哈希和大小正确，文件名用于展示和 eMule 侧兼容。

### 2.3 115 对 ED2K 链接的处理方式

关键证据：

1. **115 社区 T787020**（[链接](https://115.com/115/T787020.html)）：
   - 用户添加一个 ED2K 链接，离线完成后文件名却是 115 服务端另一个名字；
   - 回帖：“因为他不是真下是服务器内找文件所以自定义不了名字，真下就可以自定义名用下载器一试就知。”
   - 说明 115 会优先在服务端按哈希找文件，命中后直接转存，并沿用服务端已有的文件名。

2. **fake115uploader issue #48**（[链接](https://github.com/orzogc/fake115uploader/issues/48)）：
   - 2023-03-08 的评论：“是的，秒链弄成 ed2k 也一样可以秒离，主要还是上传。”
   - 说明把上传后的文件转成 ED2K 链接，115 可以“秒离”（瞬间完成离线转存）；前提是文件已经上传到 115。

3. **115 官方“什么是云下载”**（[链接](https://115.com/115115/T895745.html)）：
   - 云下载本质是 115 服务器代用户下载；
   - 如果服务端没有命中，就会走真实下载，成功与否取决于供源。

4. **115 社区 T972188**（[链接](https://115.com/115/T972188.html)）：
   - 客服回复“云下载成功与否取决于链接的供源情况”“并不是简单地通过服务器判断是否有相同文件进行识别转存”；
   - 这与 T787020 并不矛盾：115 会先查服务端资源，命中才秒转；没命中就真实下载。不能假设“只要上传过就一定秒离”。

**结论**：本功能应保证链接的 **哈希、大小、格式** 正确；115 能否秒离取决于 115 服务端是否已有对应资源。对本工具刚上传的文件，通常应有较大概率命中，但需要实测确认，且不能把它写成绝对保证。

### 2.4 115 离线接口（用于可选的自动验证）

来自 [115bot-go 的 offline.go](https://github.com/AidanAllen/115bot-go/blob/main/internal/p115/offline.go) 和 [py115 文档](https://py115.readthedocs.io/en/latest/example.common.html)：

- 添加任务：
  - `POST https://115.com/web/lixian/?ct=lixian&ac=add_task_urls`
  - 表单：`url[0]=<链接>`、`url[1]=...`、`wp_path_id=<保存目录 cid>`
  - 注意字段名是 `url[i]` 数组，不是 `urls`。
- 任务列表：
  - `https://115.com/web/lixian/?ct=lixian&ac=task_lists&page=N`
  - 返回任务含 `info_hash`、`status`（`2` 表示完成）、`file_id`、`name`、`url` 等。
- 删除任务：
  - `POST https://115.com/web/lixian/?ct=lixian&ac=task_del`
  - 表单：`hash[0]=<info_hash>`、`flag=1`（同时删文件）。

这些接口可以作为「生成后自检」的辅助手段，但会消耗 115 离线配额、创建实际文件，**不建议放在默认上传流程里**。建议做成显式的 `-ed2k-verify` 或独立的测试脚本。

### 2.5 可借鉴的项目/工具汇总

| 项目/工具 | 可借鉴点 |
| --- | --- |
| [RHash](https://github.com/rhash/RHash/blob/master/librhash/ed2k.c) | eMule 兼容算法、整数倍边界处理 |
| [Kimundi/ed2k](https://github.com/Kimundi/ed2k) | Red/Blue 定义、测试向量 |
| [zorchenhimer/go-ed2k](https://github.com/zorchenhimer/go-ed2k) | Go 的 `SumBlue`/`SumRed` 实现 |
| [go.felesatra.moe/hash/ed2k](https://pkg.go.dev/go.felesatra.moe/hash/ed2k) | `hash.Hash` 风格、`AltHash` |
| [chocko124/ed2k-link-generator-toolkit](https://github.com/chocko124/ed2k-link-generator-toolkit) | 流式、Blue/Red、原始文件名、115/PikPak 场景说明 |
| [ed2ktool.com](https://www.ed2ktool.com/) | 浏览器端本地计算的产品形态 |
| [mrx.la ED2K.HashTool.Wpf](https://mrx.la/921) | Windows 批量工具，115 用户的实际工作流；提到带 `h=` 标签的链接 |
| [115bot-go](https://github.com/AidanAllen/115bot-go) | 115 离线 API 的实测字段与坑 |
| [py115](https://py115.readthedocs.io/en/latest/example.common.html) / [p115client](https://github.com/ChenyangGao/p115client) | 115 离线接口封装，确认支持 ed2k 输入 |

**没有发现 115 提供“根据文件返回 ed2k 链接”的公开 API**；可行路线就是在本地计算哈希并拼接链接。

---

## 3. 实现方案

### 3.1 总体设计

```text
上传文件
  │
  ├─ 打开文件，一次顺序读取
  │    ├─ SHA1（现有逻辑）
  │    └─ ED2K（新增：分块 MD4，同时得到 Blue/Red）
  │
  ├─ 秒传 / OSS / 分片上传（现有逻辑）
  │    └─ fastToken 携带 SHA1、大小、ED2K
  │
  ├─ 上传成功
  │    ├─ result.Success 追加路径（保持原样）
  │    └─ result.ED2K 追加 {path,name,size,hash,link}
  │
  └─ exitPrint
       ├─ 控制台打印 ED2K 段落
       ├─ result.json 写入 ed2k 数组
       └─ 可选写入 时间 ed2k.txt（一行一个链接）
```

核心原则：

- **只读一次文件**：用 `io.MultiWriter(sha1, ed2kHasher)` 同时喂给两个哈希。
- **默认关闭**：`-ed2k` 或配置 `"ed2k": true` 才计算，避免所有用户多付 MD4 的 CPU。
- **结果向后兼容**：不改 `success` 类型，新增 `ed2k` 字段。
- **边界情况双链接**：文件大小是 9,728,000 整数倍时，同时给出 Blue/Red，避免猜错。

### 3.2 新增 `ed2k.go`

建议新增一个独立文件，职责单一，便于单测。

```go
package main

import (
    "fmt"
    "hash"
    "strconv"
    "strings"

    "golang.org/x/crypto/md4"
)

const ed2kChunkSize = 9728000 // 9,500 KiB

// ed2kHasher 流式计算 ED2K；内部保存每个完整块的 MD4。
type ed2kHasher struct {
    chunk   hash.Hash // 当前块的 MD4
    filled  int       // 当前块已写入字节数
    digests []byte    // 所有完整块的 MD4 拼接
    size    int64     // 总字节数
}

func newED2KHasher() *ed2kHasher {
    return &ed2kHasher{chunk: md4.New()}
}

func (h *ed2kHasher) Write(p []byte) (int, error) {
    n := len(p)
    h.size += int64(n)
    for len(p) > 0 {
        space := ed2kChunkSize - h.filled
        if space > len(p) {
            space = len(p)
        }
        _, _ = h.chunk.Write(p[:space])
        h.filled += space
        p = p[space:]

        if h.filled == ed2kChunkSize {
            h.digests = h.chunk.Sum(h.digests)
            h.chunk.Reset()
            h.filled = 0
        }
    }
    return n, nil
}

type ed2kSums struct {
    Size     int64
    Blue     string
    Red      string
    Boundary bool // 大小是 ed2kChunkSize 的正整数倍
}

func (h *ed2kHasher) Sums() ed2kSums {
    digests := h.digests
    if h.filled > 0 || len(digests) == 0 {
        // 最后一块不满，或空文件：把当前块的 MD4 补进列表
        digests = h.chunk.Sum(digests)
    }

    s := ed2kSums{
        Size:     h.size,
        Boundary: h.size > 0 && h.size%ed2kChunkSize == 0,
    }
    s.Blue = ed2kRoot(digests)
    s.Red = s.Blue
    if s.Boundary {
        empty := md4.New()
        all := append(append([]byte{}, digests...), empty.Sum(nil)...)
        s.Red = ed2kRoot(all)
    }
    return s
}

func ed2kRoot(digests []byte) string {
    if len(digests) == 16 {
        return strings.ToUpper(fmt.Sprintf("%X", digests))
    }
    h := md4.New()
    _, _ = h.Write(digests)
    return strings.ToUpper(fmt.Sprintf("%X", h.Sum(nil)))
}

func ed2kLink(name string, size int64, hash string) string {
    return "ed2k://|file|" + name + "|" +
        strconv.FormatInt(size, 10) + "|" + strings.ToUpper(hash) + "|/"
}
```

要点：

- **内存**：`digests` 每 9.7 MB 只增长 16 字节；115 GB 文件约 189 KB，可忽略。
- **空文件**：`Sums()` 会补 `MD4("")`，得到 `31D6...`，不会越界。
- **整数倍**：`Boundary` 为真时算 Red；Blue 直接使用块哈希拼接。
- **单块文件**：`len(digests)==16` 直接返回，不再做根哈希（符合算法）。
- **文件名**：直接使用原始 UTF-8，不做 URL 编码；中文、空格、`[` `]`、全角符号都保持原样。`|` 是字段分隔符，文件名不能包含它（Windows 文件名也不允许）。
- **可选**：如果希望链接带 `h=`（AICH），需要额外实现 AICH 哈希树；115 不要求，v1 不做。

### 3.3 改造 `hash.go`：一次读取同时算 SHA1 和 ED2K

新增一个结构体和函数，替代 `uploadFileSHA1()` 中对 `hashSHA1()` 的调用：

```go
type fileHashes struct {
    SHA1     string
    Size     int64
    ED2K     ed2kSums
}

func hashFile(f *os.File, withED2K bool) (fileHashes, error) {
    var r fileHashes

    // 保留原来的前 128 KiB blockHash 逻辑（目前未使用，可保留或删除）
    // ...

    if _, err := f.Seek(0, io.SeekStart); err != nil {
        return r, err
    }

    sha1h := sha1.New()
    var w io.Writer = sha1h
    var eh *ed2kHasher
    if withED2K {
        eh = newED2KHasher()
        w = io.MultiWriter(sha1h, eh)
    }

    n, err := io.Copy(w, f)
    if err != nil {
        return r, err
    }
    r.Size = n
    r.SHA1 = strings.ToUpper(hex.EncodeToString(sha1h.Sum(nil)))
    if eh != nil {
        r.ED2K = eh.Sums()
    }
    return r, nil
}
```

说明：

- `hashSHA1()` 可以保留为兼容包装，或直接替换调用点；`hashFileRange()` 不变。
- 原来的 `f.Read(block)` + `checkErr(err)` 对空文件会 panic，新实现用 `io.Copy` 不会。
- 如果担心文件在哈希期间被修改，可在最后 `f.Stat()` 比较大小；现有代码没有这个保护，v1 可不做。
- `blockHash` 目前没有调用方使用，可顺手删掉，减少一次 128 KiB 读取；但为了最小改动，也可以保留。

### 3.4 改造 `fast.go`：让 `fastToken` 携带 ED2K 元数据

`fastToken` 增加字段（带 JSON tag，便于分片存档持久化）：

```go
type fastToken struct {
    // ... 现有字段 ...
    SHA1       string
    Size       int64  `json:"ed2kSize,omitempty"`
    FileName   string `json:"ed2kFileName,omitempty"`
    ED2KBlue   string `json:"ed2kBlue,omitempty"`
    ED2KRed    string `json:"ed2kRed,omitempty"`
    ED2KBoundary bool `json:"ed2kBoundary,omitempty"`
}
```

`uploadFileSHA1()` 改为：

```go
func (file *fileInfo) uploadFileSHA1() (body []byte, hashes fileHashes, e error) {
    f, err := os.Open(file.Path)
    checkErr(err)
    defer f.Close()

    hashes, err = hashFile(f, config.ED2K)
    checkErr(err)

    info, err := os.Stat(file.Path)
    checkErr(err)
    hashes.Size = info.Size() // 以实际大小为准确认

    file.SHA1 = hashes.SHA1
    file.Size = hashes.Size
    file.Name = info.Name()
    file.ED2KBlue = hashes.ED2K.Blue
    file.ED2KRed = hashes.ED2K.Red
    file.ED2KBoundary = hashes.ED2K.Boundary

    body, err = uploadSHA1(info.Name(), strconv.FormatInt(info.Size(), 10),
        hashes.SHA1, "", "", file.ParentID)
    checkErr(err)
    // status 7/701 的 sign_check 逻辑保持不变
    return body, hashes, nil
}
```

`fastUploadFile()` 在拿到 body 后，把元数据同步到 token：

```go
token.SHA1 = hashes.SHA1
token.Size = hashes.Size
token.FileName = file.Name
token.ED2KBlue = file.ED2KBlue
token.ED2KRed = file.ED2KRed
token.ED2KBoundary = file.ED2KBoundary
```

这样：

- `-f` 秒传成功：`file` 上已有元数据，直接生成链接。
- `-u`/`-m` 秒传失败：返回的 `token` 已有元数据，后续 OSS/分片成功后可生成链接。
- `-m` 存档：`saveProgress` 序列化 `fastToken` 时会自动带上新字段。

### 3.5 改造 `multipart.go`：恢复上传时兼容旧存档

`saveProgress` 结构不变，`FastToken` 会自动带上新字段。恢复时：

```go
if sp != nil {
    ft = sp.FastToken
    // 旧存档没有 ED2K 字段：重算一次
    if config.ED2K && ft.ED2KBlue == "" {
        f, err := os.Open(file)
        checkErr(err)
        hashes, err := hashFile(f, true)
        f.Close()
        checkErr(err)
        ft.SHA1 = hashes.SHA1
        ft.Size = hashes.Size
        ft.ED2KBlue = hashes.ED2K.Blue
        ft.ED2KRed = hashes.ED2K.Red
        ft.ED2KBoundary = hashes.ED2K.Boundary
    }
}
```

`resumeUpload()` 建议返回 token，方便调用方取元数据：

```go
func resumeUpload(file *fileInfo) (*fastToken, error) {
    sp := new(saveProgress)
    err := multipartUploadFile(nil, file.Path, file.ParentID, sp)

    return sp.FastToken, err
}
```

`main.go` 中恢复成功后将 token 的元数据写回 `file`：

```go
token, err := resumeUpload(file)
if err == nil && token != nil {
    file.setFromToken(token)
}
```

如果不想改 `resumeUpload` 签名，也可以在成功后重新对本地文件算一次 ED2K；但大文件会多一次完整读盘，不推荐。

**顺手修正**：`oss.go:200` 和 `multipart.go:252` 的校验请求目前用 `config.CID`，应改为 `file.ParentID`。这需要把 `ossUploadFile`/`multipartUploadFile` 的参数从 `file string` 改为 `*fileInfo`（或额外传入 cid）。如果本次不想扩大改动，可以单独提一个 fix。

### 3.6 改造 `main.go`：元数据、结果、参数

#### 3.6.1 `fileInfo` 增加字段

```go
type fileInfo struct {
    Path         string `json:"path"`
    ParentID     uint64 `json:"parentID"`
    SHA1         string
    Name         string
    Size         int64
    ED2KBlue     string
    ED2KRed      string
    ED2KBoundary bool
}
```

#### 3.6.2 结果结构

```go
type ed2kResult struct {
    Path    string `json:"path"`
    Name    string `json:"name"`
    Size    int64  `json:"size"`
    Variant string `json:"variant"` // blue / red / both
    Hash    string `json:"hash"`    // 主链接哈希
    Link    string `json:"link"`    // 主链接
    HashBlue string `json:"hashBlue,omitempty"`
    LinkBlue string `json:"linkBlue,omitempty"`
    HashRed  string `json:"hashRed,omitempty"`
    LinkRed  string `json:"linkRed,omitempty"`
}

type resultData struct {
    Success []string     `json:"success"`
    Failed  []failedFile `json:"failed"`
    Saved   []string     `json:"saved"`
    ED2K    []ed2kResult `json:"ed2k,omitempty"`
}
```

`success` 保持 `[]string`，现有消费者不受影响；需要链接的消费者读 `ed2k`。

#### 3.6.3 `uploadConfig` 增加字段

```go
type uploadConfig struct {
    // ... 现有字段 ...
    ED2K     bool   `json:"ed2k"`     // 是否生成 ED2K 链接
    ED2KMode string `json:"ed2kMode"` // auto / blue / red
    ED2KFile string `json:"ed2kFile"` // 可选：链接输出文件
}
```

默认值处理：

- 读取旧配置后，若 `ED2KMode == ""`，设为 `auto`。
- 若 `ED2K` 为 true，则默认启用。

#### 3.6.4 CLI 参数

在 `initialize()` 中新增：

```go
ed2k := flag.Bool("ed2k", false, "为上传成功的文件生成 ED2K 链接")
ed2kMode := flag.String("ed2k-mode", "auto", "ED2K 哈希模式：auto、blue、red")
ed2kFile := flag.String("ed2k-file", "", "将 ED2K 链接保存到指定`文件`")
```

优先级：**命令行 > 配置文件 > 默认值**。需要区分「命令行没传」和「命令行显式传 false」时，用 `flag.Visit` 判断；简单实现可以只做「传了 `-ed2k` 就覆盖为 true」。

校验：

- `auto|blue|red` 之外报错退出。
- `-ed2k-file` 非空但没开 `-ed2k` 时，自动开启或报错（建议自动开启，减少用户困惑）。
- 输出目录必须存在（复用 `ResultDir` 的检查逻辑）。

#### 3.6.5 成功时收集链接

在 `uploadFile()` 的每个成功分支（`-f`、`-u`、`-m` 非恢复、`-m` 恢复）调用：

```go
file.addED2KResult()
```

实现：

```go
func (file *fileInfo) addED2KResult() {
    if !config.ED2K || file.ED2KBlue == "" {
        return
    }
    name := file.Name
    if name == "" {
        name = filepath.Base(file.Path)
    }
    size := file.Size

    r := ed2kResult{
        Path: file.Path, Name: name, Size: size,
        Variant: "blue",
        Hash: file.ED2KBlue,
        Link: ed2kLink(name, size, file.ED2KBlue),
    }

    switch config.ED2KMode {
    case "red":
        r.Variant, r.Hash, r.Link = "red", file.ED2KRed, ed2kLink(name, size, file.ED2KRed)
    case "auto":
        if file.ED2KBoundary {
            r.Variant = "both"
            r.HashBlue, r.LinkBlue = file.ED2KBlue, ed2kLink(name, size, file.ED2KBlue)
            r.HashRed, r.LinkRed = file.ED2KRed, ed2kLink(name, size, file.ED2KRed)
            // 主链接给 Red：115 的 ED2K 生态更接近 eMule，Red 是更可能的兼容项；
            // Phase 0 实测后可把这里改成 Blue。
            r.Hash, r.Link = r.HashRed, r.LinkRed
        }
    }

    result.ED2K = append(result.ED2K, r)
}
```

非边界时 Blue == Red，只输出一个；边界时 `auto` 输出两个，主链接可切换。

#### 3.6.6 `exitPrint()` 输出

在现有成功/失败/存档打印之后增加：

```go
if len(result.ED2K) != 0 {
    fmt.Printf("ED2K 链接（%d）：\n", len(result.ED2K))
    for _, e := range result.ED2K {
        fmt.Printf("%s\n%s\n", e.Path, e.Link)
        if e.Variant == "both" {
            fmt.Printf("  blue: %s\n", e.LinkBlue)
        }
    }
}
```

同时：

- `result.json` 已经包含 `ed2k` 数组（`json.MarshalIndent(result)` 自动带上）。
- 如果 `config.ED2KFile != ""`，写入该文件；
- 如果没指定 `ED2KFile` 但指定了 `ResultDir`，默认写 `ResultDir/时间 ed2k.txt`；
- 文件格式：一行一个主链接；边界时可写两行（red 在前，blue 在后），或写成 `path<TAB>link`，按需要定。建议纯链接，便于复制。

```go
func writeED2KLinks(path string) error {
    var b strings.Builder
    for _, e := range result.ED2K {
        b.WriteString(e.Link)
        b.WriteByte('\n')
        if e.Variant == "both" {
            b.WriteString(e.LinkBlue)
            b.WriteByte('\n')
        }
    }
    return os.WriteFile(path, []byte(b.String()), 0644)
}
```

### 3.7 115 兼容性策略

| 问题 | 策略 |
| --- | --- |
| 文件名 | 默认用本地文件名，原始 UTF-8 写入，不做 URL 编码；115 匹配时忽略文件名，不影响秒离 |
| 文件大小 | 用本地实际字节数；与 115 存储不一致说明上传异常，应记录警告 |
| 哈希大小写 | 统一大写 |
| Blue/Red | 非边界无差异；边界默认 `auto` 同时输出两个，主链接先用 Red，实测后调整 |
| `h=` AICH | 不输出；115 不要求，带 `h=` 的链接也能被 115 接受 |
| 文件名里的 `|`、`%`、空格、中文 | 原始写入；`|` 是字段分隔符，文件名不能包含（Windows 文件名也不允许） |
| 115 改名/加广告名 | 115 使用服务端已有名字；链接名仅用于展示，不影响匹配 |
| 115 没命中服务端资源 | 会走真实下载，取决于 ED2K 供源；文档中明确说明，不承诺 100% 秒离 |

### 3.8 边界情况清单

| 场景 | 处理 |
| --- | --- |
| 空文件 | `hashFile` 用 `io.Copy` 不再 panic；ED2K 为 `MD4("")`；链接正常生成 |
| 文件 < 9,728,000 字节 | 单块，Blue=Red |
| 文件 = 9,728,000 字节 | 边界，Blue/Red 不同；`auto` 输出两个 |
| 文件 > 9,728,000 且非整数倍 | 多块，Blue=Red |
| 超大文件（> 9 GB） | 分片上传逻辑不变；ED2K 内存只随块数线性增长（189 KB/115 GB） |
| `-e` 删除源文件 | ED2K 在删除前已算完并保存在 `file`/`token` 中 |
| `-m` 断点恢复 | 新存档带 ED2K；旧存档检测到空字段时重算一次 |
| 同一文件重复上传 | 链接不变；115 可能秒离到已有文件，属于预期 |
| 递归上传文件夹 | 每个文件单独生成链接；结果 `path` 保留原路径 |
| 上传失败 | 不生成链接；`result.Failed` 行为不变 |
| `-ed2k` 关闭 | 不计算 MD4，不新增输出，完全保持旧行为 |
| 结果 JSON 兼容 | `success` 不变，新增可选 `ed2k` 数组 |
| 文件名含换行/控制字符 | 原始写入；换行会破坏 `ed2k.txt` 的行结构（Windows 文件名不允许控制字符） |
| 文件名超长 | 链接可能很长，但 115 接受；必要时可加 `-ed2k-name` 用短名，但会影响可读性 |

---

## 4. 测试方案

### 4.1 单元测试（新增 `ed2k_test.go`）

1. **官方/公开向量**
   - 空文件、`"a"`、`"abc"`、`"hello world"`；
   - 9,728,000 个 `0x55` 的 Blue/Red 向量。
2. **分块边界**
   - 9,728,000 - 1、9,728,000、9,728,000 + 1；
   - 2 × 9,728,000；
   - 用较小的自定义 chunk size 做性质测试（可选，需要把常量参数化）。
3. **流式一致性**
   - 同一份数据按 1 字节、7 字节、32 KiB、1 MiB 等不同 `Write` 切分，结果一致。
4. **链接文件名**
   - 中文、空格、`[` `]`、全角符号、`%`、`?`、`#` 等保持原始 UTF-8；
   - 断言输出形如 `ed2k://|file|中文 文件.zip|42|AAAA...|/`，不出现 `%20`/`%5B` 等编码。
5. **`hashFile`**
   - 同时校验 SHA1/大小/ED2K；
   - 空文件不再 panic；
   - `withED2K=false` 时 ED2K 为空、SHA1 正确。
6. **`addED2KResult`**
   - `auto` 非边界只产生一个；
   - `auto` 边界产生 `both` 且两个链接不同；
   - `blue`/`red` 只产生对应链接；
   - `-ed2k` 关闭时不产生。

### 4.2 外部交叉验证

- Linux/macOS 可用 `rhash --ed2k`（eMule 兼容，Red）对比边界文件；
- 用 `ed2ktool --mode blue` / `--mode red` 对比；
- 随机生成 0 B、1 B、1 KiB、10 MiB、9,728,000 B、9,728,001 B 文件，批量比对；
- 非边界文件 Blue/Red 应相同；边界文件应分别匹配对应工具。

### 4.3 115 端到端验证（关键）

准备：

- 115 账号（最好有离线配额/VIP）；
- 一个测试文件夹 cid；
- 测试文件：
  1. 随机小文件（非边界）；
  2. 恰好 9,728,000 字节的文件（边界，用于判定 Blue/Red）；
  3. 9,728,000 + 1 字节的文件（多块非边界）。

步骤：

1. 用 `-u` 或 `-m` 上传到测试文件夹，记录生成的链接；
2. 在 115 网页「云下载」中粘贴链接，保存到测试文件夹；
3. 观察任务：
   - 若几秒内完成、文件出现 → 115 服务端命中，链接可用；
   - 若显示下载中/等待/失败 → 115 没命中，正在真实下载；记录状态和 `peers`；
4. 对边界文件分别测试 `blue` 和 `red` 两个链接，确定 115 服务端实际使用哪个变体；
5. 用 115 离线 API 自动化（可选）：
   - `add_task_urls` 添加；
   - 轮询 `task_lists`，看 `info_hash` 对应任务的 `status` 是否很快变成 `2`；
   - `task_del` + `flag=1` 清理测试任务和文件。

判定标准：

- **格式可用**：115 不返回「错误的链接」（errcode 10004）；
- **秒离可用**：任务快速完成，且 `file_id` 非空；
- **哈希变体**：边界文件只有一个变体能秒离，另一个会进入下载/失败。

如果两个变体都不能秒离：

- 先确认文件已完整上传（SHA1 校验通过）；
- 等待一段时间再试（115 索引可能有延迟）；
- 换一个 115 账号/文件夹排除配额或风控；
- 若仍不行，说明 115 没有为该文件建立 ED2K 索引；此时链接仍是合法的 ED2K 标识，但“115 秒离”不能保证，应在文档中如实说明。

### 4.4 回归测试

- `-f`/`-u`/`-m` 三种模式在 `-ed2k` 关闭时行为完全不变；
- `result.json` 旧字段不变；
- `-e` 删除文件后链接仍能输出；
- 断点续传：上传中途按 `q`，重启后恢复，最终链接正确；
- 递归上传：每个文件都有链接，路径正确；
- 代理、`-http-retry`、`-parts-num` 等现有参数不受影响。

---

## 5. 分阶段任务与工作量

| 阶段 | 内容 | 产出 | 预估 |
| --- | --- | --- | --- |
| Phase 0 | 用 115 实测 Blue/Red 和秒离行为 | 测试结论、默认模式 | 0.5–1 天（需要 115 账号） |
| Phase 1 | 新增 `ed2k.go` + 单元测试 | 可独立验证的哈希/链接实现 | 0.5 天 |
| Phase 2 | 改造 `hash.go`，一次读取算 SHA1+ED2K | `hashFile`、空文件修复 | 0.5 天 |
| Phase 3 | 改造 `fast.go`/`multipart.go`，元数据随 token/存档传递 | 三种模式都能拿到 ED2K | 1 天 |
| Phase 4 | 改造 `main.go`：参数、config、结果、输出文件 | 完整 CLI 功能 | 1 天 |
| Phase 5 | 结果输出/README/兼容性处理 | 文档、示例 | 0.5 天 |
| Phase 6 | 回归 + 115 E2E + 边界测试 | 测试报告 | 1 天 |
| 合计 |  |  | **约 5–6 天** |

如果 Phase 0 发现 115 不索引上传文件，则需要重新评估功能定位（仅生成链接 vs 保证秒离），但 ED2K 生成本身仍可实现。

---

## 6. 风险与备选方案

### 6.1 主要风险

1. **115 服务端未索引该文件**（最高风险）
   - 上传成功 ≠ 一定秒离；115 可能走真实下载。文档必须说明，不要承诺 100%。
   - 缓解：Phase 0 实测；提供 `-ed2k-verify` 可选自检；失败时给出明确提示。

2. **Blue/Red 变体**
   - 只影响 9,728,000 整数倍的文件；默认双链接可规避。
   - 缓解：Phase 0 用边界文件实测；`-ed2k-mode` 可手动切换。

3. **性能**
   - MD4 是额外 CPU 开销；超大文件秒传时 SHA1+MD4 可能接近两倍哈希时间。
   - 缓解：`-ed2k` 默认关闭；一次读盘；未来可并行分块 MD4（每块独立，可 worker pool）。

4. **115 文件名/大小与本地不一致**
   - 115 可能改名、截断、替换字符；但匹配按哈希，不影响秒离。
   - 缓解：链接默认用本地名；如需展示 115 名，可后续加 `-ed2k-name 115` 并通过文件列表接口查询。

5. **旧存档兼容**
   - 旧 `saveProgress` 没有 ED2K 字段；恢复时需重算。
   - 缓解：检测空字段后重算；或在恢复前提示用户。

6. **结果 JSON 消费者**
   - 新增字段是增量的；但如果有消费者严格校验字段集合，需要版本说明。
   - 缓解：`ed2k` 使用 `omitempty`，关闭时不出现。

7. **115 接口变化**
   - 核心功能不依赖 115 新接口；可选的自动验证依赖 `web/lixian`，可能变化。
   - 缓解：自动验证做成独立可选功能，不影响主流程。

8. **合规/安全**
   - MD4 已不适合安全用途；ED2K 链接只用于文件标识。
   - 文档中注明：仅用于合法文件管理，遵守 115 服务条款。

### 6.2 备选方案

| 方案 | 优点 | 缺点 | 建议 |
| --- | --- | --- | --- |
| A. 上传成功后再读一遍算 ED2K | 集成简单，不改上传路径 | 多一次完整读盘；`-e` 已删文件时不可用；大文件慢 | 不推荐 |
| B. 调用外部 `rhash`/`ed2ktool` | 不用自己实现 | 增加外部依赖，跨平台分发麻烦 | 不推荐 |
| C. 找 115 的“生成 ed2k”接口 | 最省事 | 调研未发现公开接口；不稳定 | 不可行 |
| D. 一次读取同时算 SHA1+ED2K（本方案） | 无额外 I/O，性能最好 | 需要改哈希/上传路径 | **推荐** |
| E. 默认开启 ED2K | 用户无感 | 所有用户多付 CPU；秒传大文件明显变慢 | 不推荐，默认关闭 |
| F. 边界时只输出一个变体 | 输出简洁 | 可能选错，115 不命中 | 不推荐，边界输出两个 |

---

## 7. 建议的落地顺序（PR 拆分）

1. **PR 1：`ed2k.go` + 测试**
   - 只新增文件，不改现有逻辑；
   - 用公开向量验证算法；
   - 容易 review。

2. **PR 2：`hash.go` 一次读取 + 空文件修复**
   - 新增 `hashFile`，保留旧接口；
   - 先不接入上传流程。

3. **PR 3：`fast.go`/`multipart.go` 元数据传递**
   - `fastToken` 增加字段；
   - 存档兼容；
   - 三种模式都能拿到 ED2K。

4. **PR 4：`main.go` CLI/结果/输出**
   - `-ed2k`、`-ed2k-mode`、`-ed2k-file`；
   - `result.ED2K`；
   - 控制台/文件输出。

5. **PR 5：文档 + 115 E2E 测试报告**
   - README 增加使用说明和限制；
   - 记录 Blue/Red 实测结论和 115 行为。

---

## 8. 参考资料

- [RHash ed2k.c（eMule 兼容实现）](https://github.com/rhash/RHash/blob/master/librhash/ed2k.c)
- [Kimundi/ed2k（Red/Blue 定义与测试向量）](https://github.com/Kimundi/ed2k)
- [zorchenhimer/go-ed2k](https://github.com/zorchenhimer/go-ed2k)
- [go.felesatra.moe/hash/ed2k](https://pkg.go.dev/go.felesatra.moe/hash/ed2k)
- [chocko124/ed2k-link-generator-toolkit](https://github.com/chocko124/ed2k-link-generator-toolkit)
- [ED2K Tool 在线生成器](https://www.ed2ktool.com/)
- [mrx.la：ED2K.HashTool.Wpf（115 场景）](https://mrx.la/921)
- [115 社区：电驴 ED2K 链接添加离线任务逻辑（文件名/服务端找文件）](https://115.com/115/T787020.html)
- [115 社区：离线失败与供源讨论](https://115.com/115/T972188.html)
- [115 官方：什么是云下载](https://115.com/115115/T895745.html)
- [fake115uploader issue #48（“秒链弄成 ed2k 也一样可以秒离”）](https://github.com/orzogc/fake115uploader/issues/48)
- [115bot-go：115 离线接口实现](https://github.com/AidanAllen/115bot-go/blob/main/internal/p115/offline.go)
- [py115 文档：Add Task 支持 ed2k](https://py115.readthedocs.io/en/latest/example.common.html)
- [golang.org/x/crypto/md4](https://pkg.go.dev/golang.org/x/crypto/md4)
