# 操作指南

[English](user-guide.md) · **简体中文**

本文面向使用控制台的两类人：把路由器搭起来并维护下去的**管理员**，以及登录、拿到密钥、把客户端指
过来的**普通用户**。下面的每一张截图都来自真实控制台，版本 1.0.0，界面语言为英文。

第一次阅读请按顺序看。管理员部分放在前面，因为在后端连接、模型目录和密钥策略配置好之前，普通用户
无事可做。

> 截图使用的是英文界面。控制台自带简体中文语言（顶栏的语言选择器可切换），因此正文在关键位置会同时
> 给出中文说明和界面上的英文原文，方便对照截图查找。本文引用的其他深入文档目前只有英文版。

---

## 1. 系统概述

### 1.1 它做什么

Model Router 接收兼容 OpenAI 协议的 `POST /v1/chat/completions` 请求，并把每一个请求转发给**合适
的**后端模型，而不是固定的那一个。“合适”由你写的规则决定，或者由一个小的决策模型决定，或者两者共
同决定；每一次决策都被完整记录下来，事后可以查证。

| 能力 | 在控制台中的位置 |
|---|---|
| 按规则路由、按 AI 决策路由，或两者结合 | Routing configuration → Routing strategy / Rule routing |
| 同时使用多个后端，Azure AI Foundry 或任何兼容 OpenAI 的地址 | Routing configuration → Backend connections |
| 每一次用户交互做一次路由决策，而不是每一个 HTTP 请求做一次 | Routing configuration → Routing strategy（会话粘性） |
| 把每一次调用归属到具体的人 | API keys（密钥的所有者就是调用者身份） |
| 按 GitHub Enterprise / 组织 / Enterprise Team 控制谁可以创建密钥 | Access control → Key policy |
| 控制每个用户、团队、组织分别可以调用哪些模型 | Model policy |
| 查看请求、决策过程、后端调用和响应 | Traces |
| 按模型、按天、按用户看调用量、Token、错误率和延迟 | Usage |

### 1.2 工作原理

只有一个进程。它在 `/` 提供控制台，在 `/v1` 下提供兼容 OpenAI 的 API，并把全部状态放在一个 `data/`
目录里：没有数据库，没有缓存服务，没有队列。一个请求依次经过身份认证、模型策略、粘性绑定检查、路由
策略、针对所选模型的参数适配、后端调用，最后写入调用链记录。

架构图见 [Architecture and data flow](architecture.md)；请求路径的逐步说明见
[Router logic](router-logic.md)。本文是同一套系统的操作视角：按哪个按钮、按什么顺序、按完之后屏幕
上会显示什么。

### 1.3 动手之前值得先理解的几个概念

| 概念 | 在这里的含义 |
|---|---|
| **连接**（connection / provider） | 一个后端地址加上它的密钥。模型绑定到连接上，所以一个路由器可以同时服务位于不同地址的模型。 |
| **模型目录**（model catalog） | 客户端允许在请求里写的模型名。每一项都带一段描述，AI 决策模型选型时读的就是这段描述。 |
| **默认模型**（default model） | 没有规则命中时使用，AI 决策失败或超时时也使用。有且只有一个模型带 `default` 标记。 |
| **交互**（interaction） | 一次用户提问。像 GitHub Copilot 这类智能体客户端会用一串 HTTP 请求来回答它，这些请求携带同一个 `x-interaction-id`。路由器为整串请求只决策一次，并把每一次调用折叠进同一条调用链记录。 |
| **调用链记录**（trace） | 一次交互的完整记录：请求参数、带证据的路由决策、每一次上游调用、以及响应。 |
| **密钥策略**（key policy） | 决定**谁可以拿到 API 密钥**，依据是 GitHub Enterprise / 组织 / Enterprise Team 成员身份。失败即拒绝：无法确认时一律不放行。 |
| **模型策略**（model policy） | 决定**已经获得授权的调用者可以使用哪些模型**。命名的模型组按作用域授予，多个授予取并集。按作用域失败即放行，完全没有任何绑定则等于不受限制。 |

这两个策略互相独立，回答的是不同的问题。密钥策略是一道权限边界（“这个人到底能不能调用 API”）；模型
策略是一种分发控制（“这个人应该看到目录里的哪些模型”）。这就是为什么前者在拿不准时拒绝，而后者不会。

### 1.4 控制台：角色、顶栏、导航

只有两种角色。**管理员**是 Access control 里列出的 GitHub 登录名，再加上本地管理员账号；**普通用户**
是其他所有能登录的人。

顶栏对两者是一样的：

![控制台顶栏](images/03-topbar.png)

从左到右：导航折叠按钮、带当前版本号的产品名、源码仓库链接、提交问题的链接、实时状态标签
（`Running · <strategy> · sticky`）、控制台语言、明暗主题切换、以及你自己的账号菜单。如果 GitHub 上
有更新的版本，这里还会出现一个更新提示标签。

管理员看到三组导航：

![管理员导航](images/05-sidenav-admin.png)

普通用户只看到前两组。整个 MANAGEMENT 分组不存在，而且它背后的接口会拒绝非管理员的会话，无论浏览器
怎么请求：

![普通用户导航](images/19-user-sidenav.png)

---

## 2. 管理员操作指南

### 2.1 首次登录

打开 `http://<主机>:8000/`。登录页会显示已经配置好的入口：

![登录页](images/01-sign-in.png)

- **Sign in with GitHub**（使用 GitHub 登录）在配置好 GitHub OAuth 应用之后出现。
- **本地管理员账号**是不依赖 github.com 的那个入口。在一个全新的容器里它是唯一的入口，因为 OAuth 的
  配置向导只对来自 `127.0.0.1` 的请求开放。

点击本地登录，出现用户名 / 密码表单：

![本地管理员登录](images/02-sign-in-local.png)

内置凭据是 **`admin` / `admin1234`**。用它登录会直接进入强制改密流程，在密码改掉之前，*控制台和管理
接口的其他任何部分都不可访问*。一个用公开的默认密码就能用的超级管理员账号，不应该是可用的。新密码至
少 8 个字符，并且不能就是内置的那个默认值。

> **如果之后被锁在外面**：把 `data/config.yaml` 里 `auth.local_admin` 下的 `password_hash` 和
> `password_salt` 清空。内置默认密码会重新生效，下次登录时控制台会再次强制改密。

登录之后，**Routing configuration** 下的四个配置步骤按彼此的依赖顺序排列，每一步都链接到下一步。

### 2.2 步骤 1 · 后端连接（Backend connections）

**Routing configuration → Backend connections。** 在至少保存一个连接之前，其他任何东西都不会工作：
一个没有可达地址的模型是没法调用的。

![步骤 1：后端连接](images/06-config-providers.png)

1. 点 **+ Add connection**，给它起个名字（`foundry`、`stub`、`eu-west`；这个名字只用来把模型绑定到
   它上面）。
2. **地址（base_url）**：Azure 用 `https://<资源名>.openai.azure.com/`；其他兼容 OpenAI 的服务要一直
   写到 `/v1`，例如 `http://127.0.0.1:8899/v1`。
3. **密钥（api_key）**：保存时写入服务端的 `data/config.yaml`。用 **Show** 可以查看当前存的是什么。
4. **接口类型（api_type）**：Azure OpenAI / AI Foundry 用 `azure`，其他一律 `openai`。
   **api_version** 只对 Azure 有意义。
5. 有一个连接带 `default` 标记；没有自己指定连接的模型会继承它。在另一行上点 **Set as default** 可
   以把默认挪过去。
6. 点 **Save and apply**。面板上方的状态条会告诉你这个页面是 *In sync with config.yaml*（与配置文件
   一致）还是有未保存的改动，**Reload** 会丢弃草稿。

保存会就地重载配置：路由器重建它的客户端池，所以改对的密钥在下一个请求就生效，不需要重启。

更多细节，包括非 Foundry 的情况：[Backend connections](providers.md)。

### 2.3 步骤 2 · 模型目录（Model catalog）

**Routing configuration → Model catalog。** 这里的名字就是客户端可以写在请求 `model` 字段里的名字。

![步骤 2：模型目录](images/07-config-models.png)

每个模型：

1. **+ Add model**，填入路由器对外暴露的名字（`gpt-4o`、`gpt-5.4-pro` 等）。
2. **描述（Description）**：值得认真写。它会作为候选目录交给 AI 决策模型，一段把*这个模型适合什么
   任务*讲清楚的描述能明显提高路由准确度。没有描述的模型，在决策模型眼里只是一个名字。
3. **连接（provider）**：*跟随默认*，或者把这一个模型绑定到别的连接上。
4. **上游名称覆盖（model_name）**：只在上游的部署名和你对外暴露的名字不一致时才需要填。
5. **默认模型（Default model）**：有且只有一个。没有规则命中时用它，AI 决策失败时也用它。
6. **推理模型（Reasoning model）**：gpt-5.x / o3 系列要勾上。勾上之后路由器会发
   `max_completion_tokens` 而不是 `max_tokens`，并去掉 `temperature` 这类采样参数，因为这些模型不接受。

删除模型时也会收拾干净：控制台会报告有多少条规则和模型组引用过它并已被更新。

### 2.4 步骤 3 · 路由策略（Routing strategy）

**Routing configuration → Routing strategy。** 三种策略，再加上粘性和决策模型的设置。

![步骤 3：路由策略](images/08-config-strategy.png)

| 策略 | 行为 | 成本 |
|---|---|---|
| **先规则，后 AI**（Rules first, then AI，推荐） | 先跑规则；命中就用该规则指定的模型。只有没被规则命中的请求才交给决策模型。 | 只为规则覆盖不到的请求付一次 LLM 调用 |
| **AI 路由**（AI routing） | 决策模型读懂每一个请求的意图然后选型。 | 每个请求多约 1 秒的一次调用 |
| **规则路由**（Rule routing） | 按顺序做关键词和提示词长度匹配；都不命中就用默认模型。 | 零 LLM 调用，1 毫秒以内 |

**会话粘性（Session stickiness）**是实际效果最大的那个开关。打开之后，一次交互中选定的模型会在这次
交互的剩余部分继续使用：智能体的工具调用循环会一直用同一个模型，只付一次路由决策的代价，而不是 N
次。请求按 `x-interaction-id` 头分组（GitHub Copilot 这类客户端本来就会发这个头），调用方自己传了
`x-session-id` 时也按它分组。TTL 控制一个空闲绑定能存活多久；**Maximum sessions** 限制存储上限，超
出后按最近最少使用淘汰。

**AI 决策模型**：选一个轻快的（比如 `gpt-4.1`），可以给它单独一个连接，并设置决策超时（超时会回落
到默认模型，而不是让请求失败）。**提示词截断长度**限制送去分类的内容长度；超过之后会从头尾各保留一
半，因为智能体提示词里真正的问题通常就在最末尾。

**决策提示词**可以在这个页面上编辑，预览由后端用路由器实际使用的那同一个函数渲染，所以预览和真实请
求会发送的内容逐字符一致，不是一个近似的拼接。这个面板还会列出没有写描述的模型，因为这些正是决策模
型无法判断的那些。

### 2.5 步骤 4 · 规则路由（Rule routing）

**Routing configuration → Rule routing。** 规则是路由中便宜且明确的那一半。

![步骤 4：规则路由](images/09-config-rules.png)

- 规则**自上而下**求值；第一个命中的决定模型，其余的不再判断。用箭头按钮调整顺序。
- **关键词（Keywords）**用逗号分隔，不区分大小写，按正则表达式匹配提示词；命中任意一个即路由。
- **最小提示词长度（Minimum prompt length）**改为按长度路由。一条规则同时写了两者时，只检查长度。
- 每条规则都指明它路由到哪个模型，并且可以停用而不删除。
- 都不命中时会发生什么，取决于策略，页面上也写着：在*规则路由*下使用默认模型；在*先规则后 AI* 下这
  个请求转交给决策模型。

规则是你在明确表达意图，所以命中的规则绝不会被决策模型二次否决。

### 2.6 模型策略：每个调用者可以用哪些模型

**Model policy**（MANAGEMENT 下的一级页面）。模型组是命名的模型集合，按作用域授予；最终结果是对该
调用者生效的所有授予的并集。

![模型策略：模型组与已登录用户](images/10-model-policy.png)

1. 勾选 **Restrict which models each caller may use** 启用策略。关闭时，每个调用者看到的都是完整目录。
2. **模型组（Model groups）**：**+ Add group**，起名，然后勾选它包含的模型。`All` / `None` 一次全选
   或全不选；`Rename` 和 `Delete` 作用于整个组。**空组是合法的**，含义也正如字面：一个调用者如果只
   被授予了一个空组，那他什么都不能调用。
3. **已登录用户的默认组（Default group for signed-in users）**把某个组授予所有登录过的人。要实现
   “只给最便宜的那个模型”，这里是最自然的位置。
4. **登录过的用户（Users who have signed in）**列出路由器见过的所有人，带首次/最近登录时间和登录次
   数，以及每个用户的组选择器。**Refresh** 会重新读取这个列表。

团队和组织的授予在同一页面再往下：

![模型策略：团队与组织绑定](images/10b-model-policy-scopes.png)

- 团队和组织来自 Access control 页面探测到的结构，所以你是从列表里选，而不是手打
  `enterprise-slug/team-id` 这种一个错字就静默匹配不到任何人的键。**+ Enter one manually** 是没有企
  业管理员令牌的部署的兜底方式。
- **may create keys** / **no keys** 标记是密钥策略透出来的信息：把一个组授予给成员根本无法创建密钥
  的作用域，等于什么也没授予，因为没有密钥他们永远到不了 `/v1/chat/completions`。不合格的作用域是列
  出来而不是隐藏，这样你能看出是密钥策略把它们挡住了，而不是结构探测失败。
- 长列表可以搜索；当 GitHub 返回的组织数少于该企业实际拥有的数量时，会出现 `partial list` 标记。
- **Save and apply** 提交，与配置页面上完全一致。

有两条规则值得记牢，正是它们让这个功能不会把部署自己锁在外面：**管理员豁免**，以及**完全没有任何绑
定的调用者不受限制**。只有真实存在、且解析结果为空集的授予才会拒绝任何东西。完整语义见
[Model policy](model-policy.md)。

### 2.7 访问控制（Access control）

四个标签页，下面的顺序就是它们重要程度的顺序。

#### 管理员与登录范围（Administrators and sign-in）

![管理员与登录范围](images/12-access-admins.png)

- **管理员 GitHub 登录名**：逗号分隔，保存后立即生效。管理员能看到管理页面、跨用户的用量和全部调用
  链记录，并且**不受密钥策略约束**（否则策略里的一个失误会把他们自己也锁在外面）。
- **登录范围（Sign-in scope）**管的是*能不能登录*，不是授权。打开时，任何 GitHub 账号都可以登录并查
  看自己那份（空的）数据；他能不能创建密钥仍然由密钥策略决定。关闭时，只有上面列出的管理员能登录。
  无论哪种情况，`/v1/chat/completions` 始终需要一个有效的 API 密钥。

> **把管理员列表清空会让所有人都失去管理员权限**，唯一的挽回办法是去改服务端的 `data/config.yaml`。

#### GitHub OAuth

![GitHub OAuth](images/13-access-oauth.png)

Client ID 和 Client Secret 来自 GitHub → Settings → Developer settings → OAuth Apps。面板上显示需要
在 GitHub 上登记的两个 URL；它们必须和这个服务实际被访问的方式一致。**Callback URL** 留空是最稳妥
的，因为留空时它由请求来源推导，并且在反向代理后面会遵循 `X-Forwarded-Proto` / `Host`。密钥保存之后
不会再回显；已存有密钥时该字段显示 `configured (leave unchanged)`。

> **这里配错会把所有人挡在登录之外，包括你自己。** 改动期间请保持本地管理员账号是启用状态。

#### 本地管理员（Local administrator）

![本地管理员](images/14-access-local-admin.png)

不依赖 github.com 的那个账号。可以启用或停用它的登录、改名、改密码。存储的只有加盐哈希，从不存密码
本身。修改凭据会让其他所有本地管理员会话登出。

> **在 GitHub OAuth 还没配置的情况下关掉它，就完全没有任何登录方式了。**

#### 密钥策略（Key policy）

这是 API 密钥创建的那道门，也是探测 GitHub 结构的页面。

![密钥策略](images/11-access-key-policy.png)

1. **密钥创建策略**：勾选 *Only allow users in the listed enterprises / organizations to create API
   keys*。关闭时，任何能登录的人都能创建密钥。
2. **GitHub Enterprise 管理员令牌**：一个带 `admin:enterprise` 权限的 Personal Access Token。
   **Verify** 会报告令牌属主、它拥有的权限范围，以及它能否列出企业。没有有效令牌时，启用的策略会拒绝
   所有人，这是刻意设计的：一个打开的策略不应该让部署比关掉它时更不安全。
3. **本地 GitHub 缓存**：企业 / 组织 / 团队结构及其成员列表保存在 `data/github/` 下，并按定时任务刷
   新，因此一次成员检查是本地的集合查找，而不是一次 GitHub 往返。这张卡片显示结构和成员列表的抓取时
   间、缓存了多少个作用域，以及每个作用域的状态。**Refresh now** 可以强制刷新。成员列表处于
   `truncated`（被截断）或 `errored`（出错）状态的作用域永远不被当作权威依据；这些检查会回落到一次
   实时探测，因为“不在我能读到的那部分名单里”并不等于“不是成员”。
4. **企业、企业团队和组织**：直接来自 GitHub API。先打开某个企业的总开关（关闭时，它下面的组织和团队
   设置完全不起作用），然后要么允许其中的*任意组织*，要么逐个勾选 **Allowed organizations** 和
   **Allowed enterprise teams**。每一行的 `allowed` / `not allowed` 标记就是最终生效的答案。
5. **Save and apply**。

完整语义，包括一个判定是如何被证据支撑的：[Access control](access-control.md)。

### 2.8 监控：用量、调用链、Playground

#### 用量（Usage）

![用量](images/04-admin-usage.png)

区间按钮（Today / Last 7 / 30 / 90 days），以及只有管理员才有的 **View scope** 选择器，可以覆盖全部
用户。四个指标块（请求数，总 Token 数并区分提示/补全，错误率并给出失败次数，平均延迟并给出 P95），
然后是按模型的请求数、按天的请求数、带 **Drill down** 的按用户表格，以及同样这些数字的表格形式。普
通用户看到的这个页面固定只统计自己。

#### 调用链（Traces）

![调用链列表](images/15-traces-list.png)

这个列表从磁盘读取并分页，所以它不局限于最近的活动：表头显示 `50 of 403`，底部可以加载更多。可以按
**Date** 过滤，按 **Trace ID** 的任意片段过滤，管理员还可以按 **User** 过滤。**Auto refresh** 只重新
加载第一页。`Decision` 列是这个模型被选中的原因：某条规则自己的名字、`default`、`ai-decision`、
`ai-fallback-default`，或者 `interaction-sticky` / `session-sticky`。`Calls` 大于 1 表示这是一个智能
体工具循环。管理员可以用行上的 `✕` 删除单条记录，也可以删除符合当前过滤条件的全部记录；两者都会要求
确认，并给出条数。

点一行，详情面板在旁边打开。拖动分隔条可以调整两栏比例，位置会被记住。

![调用链详情](images/16-trace-detail.png)

各个面板依次是概览（时间、用户、用了哪个 API 密钥、交互与会话 id、延迟拆分为决策加后端）、路由决策、
请求参数、后端调用、模型响应。JSON 以可折叠、带配色的树呈现，并提供全部展开 / 全部折叠和复制按钮。

在 `先规则后 AI` 之下，路由决策会显示**两个阶段**：`1. Rules` 列出每一条被求值的规则以及它为什么命中
或没命中，然后 `2. AI decision` 给出实际发送的系统提示词、决策输入、原始输出、理由、决策延迟和决策
Token 数。当某条规则命中时，AI 阶段干脆不存在，而这本身就是“没有为决策调用付费”的证据。

一次用了多轮上游调用的交互会显示整条链：

![一次交互，多次调用](images/17-trace-turns.png)

每一次调用都带序号、Token 用量、消息条数和 request id，面板上也明确写着它们共享同一个路由决策和同一
个模型。第一次之后的每一次调用都会带上“复用决策”的说明。

调用链格式与保留策略：[Full-chain logging](traces.md)。

#### Playground

![Playground](images/18-playground.png)

验证一次配置改动是否达到预期，这是最快的办法。粘贴一个 API 密钥（可以选择记在这个浏览器里），输入
提示词，需要时设置会话 id 来验证粘性，并选择是否流式。**Routing result** 面板给出模型、原因和决策延
迟，响应显示在它下面，**View the full trace** 可以跳到调用链记录。

Playground 调用的就是任何其他客户端调用的那个 `/v1/chat/completions`，用的也是同一个密钥，所以它显示
什么，真实调用方拿到的就是什么。

### 2.9 新部署检查清单

1. 用本地管理员登录并修改密码。
2. 添加一个后端连接并设为默认。*（步骤 1）*
3. 添加你的模型，认真写描述，指定一个默认模型，勾上推理类模型。*（步骤 2）*
4. 选一个策略，并保持会话粘性打开。*（步骤 3）*
5. 把你有把握的规则加上。*（步骤 4）*
6. 配置 GitHub OAuth 并列出管理员登录名，让除你之外的人也能登录。
7. 决定密钥策略：企业令牌、允许的组织和团队，或者干脆不启用。
8. 视需要定义模型组并授予。如果所有人都可以用所有模型，就把策略关着。
9. 在 API keys 页面创建一个密钥，并从 Playground 发一个请求。
10. 打开调用链记录，确认决策过程和你预期的一致。

---

## 3. 普通用户操作指南

### 3.1 登录

在浏览器里打开路由器的地址，选择 **Sign in with GitHub**：

![登录页](images/01-sign-in.png)

GitHub 会让你授权一次这个应用；之后你会落在自己的 Usage 页面上。你的会话是一个 Cookie，它只能访问控
制台，它不是调用 API 的凭据。如果登录被拒绝，说明管理员把登录范围限制成了只允许管理员，去申请把自己
加进去。

### 3.2 你能看到什么

导航里两个分组，里面的一切都只限于你自己：

![普通用户导航](images/19-user-sidenav.png)

| 页面 | 给你看什么 |
|---|---|
| Usage | 你自己的请求数、Token、错误率和延迟 |
| Available models | 你到底可以调用哪些模型，以及为什么 |
| API keys | 你的密钥，以及你是否可以创建 |
| Traces | 你自己的调用，完整细节 |
| Playground | 一个请求表单，不写代码也能测 |

你的用量页面和管理员看到的是同一个页面，只是范围固定为你：

![普通用户的用量页](images/20-user-usage.png)

### 3.3 你可以调用哪些模型

**Available models。** 这是权威答案，不是猜测：

![可用模型](images/21-user-models.png)

页头给出数量和原因。你可能看到的原因有：

| 原因 | 含义 |
|---|---|
| `full catalog` / 策略未启用 | 没有生效的模型策略；目录里的所有模型都可以调用 |
| `no-binding` | 策略是开着的，但没有任何授予专门指向你，所以你不受限制 |
| `union` | 你被授予了一个或多个模型组，这里是它们的并集 |
| `empty-group` | 你的授予解析后没有任何模型；在管理员改动之前调用都会被拒绝 |
| `administrator` | 你是管理员，而管理员是豁免的 |

表格列出你可以调用的每一个模型及其描述，这些描述值得读，因为路由器的决策模型替你选型时读的就是同一段
文字。`default` 标签标记的是在没有其他决定时使用的模型；`reasoning` 标记的是会忽略 `temperature` 之
类参数的模型。

你并不需要每个请求都自己挑模型。发送你列表里*任意一个*模型的名字，就足以让路由器接受并路由这个请求；
你发的是一个提示，真正决定的是路由策略。

### 3.4 创建 API 密钥

**API keys。** 顶部的面板在你动手之前就告诉你能不能创建：

![API 密钥](images/22-user-keys.png)

- **允许创建**：会说明原因（“你是组织 … 的成员，因此可以创建 API 密钥”），**Granted via** 指出到底
  是哪个作用域放你通过的。**Show evidence** 展开每个作用域的细节，包括每个答案是来自本地缓存还是一次
  实时的 GitHub 探测。**Check again** 重新评估。
- **不允许创建**：同一个面板会解释为什么，而这段话就是你向管理员申请权限时应该原文引用的内容。在控制
  台里你做任何事都改不了它。

创建密钥：起一个能让你想起它用在哪里的名字（`copilot-laptop`、`ci`），留空则叫 `default`，然后按
**Create key**。

![刚刚创建的密钥](images/23-user-key-created.png)

密钥会完整显示一次，带一个 **Copy** 按钮和一段可直接用于 GitHub Copilot BYOK 的配置。用这个密钥发出
的每一次调用都归属到你的登录名，所以请把它当作你自己的凭据：用它发出的任何内容都会以你的名字出现在调
用链记录和用量统计里。

**My keys** 表格列出每个密钥的创建时间、最近使用时间和调用次数，并且可以查看、复制、停用或删除。被停
用的密钥会以 401 被拒绝，但并没有被删除；如果你怀疑某个密钥泄露了，先停用是正确的第一步。

### 3.5 发送请求

把任何兼容 OpenAI 的客户端指向路由器。三个字段：

| 字段 | 值 |
|---|---|
| Base URL | `http://<主机>:8000/v1` |
| API Key | 你的 `mr_…` 密钥 |
| Model | Available models 页面里的任意一个模型名 |

**GitHub Copilot（BYOK）**：用上面三个值添加一个兼容 OpenAI 的 provider。Copilot 自己不发送任何用户
身份，这正是为什么归属要取自密钥的所有者；它确实会发送 `x-interaction-id`，所以它的工具调用循环只被
路由一次，并被记录成一条调用链。

**curl**：

```bash
curl http://127.0.0.1:8000/v1/chat/completions \
  -H "Authorization: Bearer mr_..." \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"Refactor this module and explain the design"}]}'
```

响应头不用打开控制台就能说明发生了什么：`x-routed-model`、`x-router-reason`、
`x-router-decision-ms`、`x-trace-id`，以及请求带了交互 id 时的 `x-router-interaction-id`。

**Playground** 就是同一个调用，只是不需要客户端：

![Playground](images/18-playground.png)

粘贴密钥，输入提示词，按 **Send**，路由结果（模型、原因、决策延迟）就出现在响应上方，并附带一个到完
整调用链记录的链接。

### 3.6 查看到底发生了什么

**Traces** 显示你自己的调用，细节和管理员看自己的调用时一样：

![调用链详情](images/16-trace-detail.png)

当某个模型的表现让你意外时，这里很有用。路由决策面板会指出是哪条规则命中的，或者给出决策模型被问了什
么、又答了什么，于是“我的问题为什么被送去了小模型”有了一个基于事实的答案。可以按日期或按调用链 id 的
片段过滤；每一次调用的 `x-trace-id` 响应头里也会返回调用链 id。

### 3.7 故障排查

| 你看到的现象 | 含义 | 怎么办 |
|---|---|---|
| `/v1/chat/completions` 返回 `401` | 密钥缺失、写错、被停用或被删除 | 检查 Authorization 头是不是 `Bearer mr_…`；在 API keys 页面确认这个密钥没有处于 `Disabled` |
| `403`，提示当前策略下没有可用模型 | 你的模型策略授予解析后是空集 | 请管理员给你授予一个模型组；Available models 页面会显示 `empty-group` |
| 创建密钥时 `403` | 密钥策略里没有包含任何你所属的作用域 | 把 API keys 面板上的原因和 **Granted via** 原文发给管理员 |
| 完全无法用 GitHub 登录 | 登录范围被限制成只允许管理员，或者 OAuth 应用配置有误 | 找管理员；他们仍然可以用本地管理员账号进去 |
| 响应里的模型不是你发的那个 | 这正是本意：路由器做了决策 | 打开调用链记录：路由决策面板会指出是哪条规则或哪次 AI 决策 |
| 一段对话里每次调用都报同一个模型和 `interaction-sticky` | 会话粘性是开着的，而你的客户端会发 `x-interaction-id` | 这是预期行为；一次交互只做一次决策 |
| Available models 里少了一个你以为有的模型 | 生效的模型策略没有授予它 | 页面上写着原因，以及是哪个授予生效了 |

---

相关文档（英文）：[Architecture and data flow](architecture.md) ·
[Router logic](router-logic.md) · [Configuration](configuration.md) ·
[Access control](access-control.md) · [Model policy](model-policy.md) ·
[Sign-in and authentication](authentication.md) · [Full-chain logging](traces.md) · [API](api.md)
