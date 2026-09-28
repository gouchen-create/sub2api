# Sub2API 异步生图与图片编辑接口

本文档描述当前生产环境已经真实验证通过的异步生图和异步图片编辑流程。

## 基本信息

- API 基础地址：`https://api.xzgc.asia`
- 创建任务：`POST /v1/images/generations/async`
- 创建编辑任务：`POST /v1/images/edits/async`
- 查询任务：`GET /v1/images/tasks/{task_id}`
- 鉴权方式：`Authorization: Bearer <SUB2API_API_KEY>`
- 请求格式：`Content-Type: application/json`
- 当前推荐模型：`gpt-image-2`

不要把 API Key 写入代码、Git、日志、提示词或聊天记录。建议通过环境变量 `SUB2API_API_KEY` 注入。

## 1. 创建异步生图任务

### 请求

```http
POST /v1/images/generations/async HTTP/1.1
Host: api.xzgc.asia
Authorization: Bearer <SUB2API_API_KEY>
Content-Type: application/json
X-Request-ID: <unique-request-id>
```

```json
{
  "model": "gpt-image-2",
  "prompt": "A miniature brass observatory on a glass platform above a midnight ocean, moonlight, cinematic product photography",
  "size": "1536x1024",
  "quality": "low",
  "n": 1,
  "response_format": "url"
}
```

### 已验证的请求字段

| 字段 | 类型 | 必填 | 推荐值或说明 |
| --- | --- | --- | --- |
| `model` | string | 推荐显式传入 | 当前使用 `gpt-image-2` |
| `prompt` | string | 是 | 生图提示词 |
| `size` | string | 否 | 已验证 `1024x1024`、`1536x1024`；也可按模型能力使用其他合法尺寸 |
| `quality` | string | 否 | `low`、`medium`、`high` 或 `auto`；低成本测试推荐 `low` |
| `n` | integer | 否 | 图片数量，必须大于 0；测试和自动化推荐固定为 `1` |
| `response_format` | string | 否 | 异步流程已验证 `url`，建议使用该值 |

当前同步 Images 解析器还支持 `background`、`output_format`、`output_compression`、`moderation`、`style` 等 OpenAI Images 字段，但这些字段尚未在异步入口逐项验收。生产调用不要把未验证字段当成稳定契约。

### 成功受理响应

HTTP 状态码为 `202 Accepted`。创建响应中至少读取 `id` 或 `task_id`，不要只判断 HTTP 202。

```json
{
  "id": "imgtask_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
  "status": "processing"
}
```

兼容读取方式：

```text
task_id = response.task_id ?? response.id
```

`X-Request-ID` 只用于日志追踪，当前不保证请求幂等，也不能替代 `task_id`。一旦拿到任务 ID，后续只能查询同一个任务，不要再次提交 POST。

## 2. 创建异步图片编辑任务

异步编辑任务复用同步 `/v1/images/edits` 的图片解析、路由、计费和对象存储流程。提交成功返回 `202 Accepted`，后续使用同一个 `task_id` 轮询；不要把编辑请求发送到 `generations/async`。

### JSON 入参

JSON 模式使用远程图片 URL。`images` 至少包含一个 `image_url`，`mask` 可选；当前实现不接受 `file_id`。

```http
POST /v1/images/edits/async HTTP/1.1
Host: api.xzgc.asia
Authorization: Bearer <SUB2API_API_KEY>
Content-Type: application/json
X-Request-ID: <unique-request-id>
```

```json
{
  "model": "gpt-image-2",
  "prompt": "Keep the person, face, hairstyle, clothing, pose, and composition unchanged. Change the city lights outside the window to warm golden sunset light.",
  "images": [
    { "image_url": "https://example.com/source.png" }
  ],
  "mask": {
    "image_url": "https://example.com/mask.png"
  },
  "size": "1024x1024",
  "quality": "low",
  "n": 1,
  "response_format": "url",
  "input_fidelity": "high",
  "output_format": "png"
}
```

### multipart 入参

本地图片使用 `multipart/form-data`：`image` 为必填源图，`mask` 可选。其余字段与 JSON 模式相同；`response_format=url` 是生产推荐值。

```bash
curl -sS -X POST "$BASE_URL/v1/images/edits/async" \
  -H "Authorization: Bearer $SUB2API_API_KEY" \
  -H "X-Request-ID: async-edit-$(date +%s)" \
  -F "model=gpt-image-2" \
  -F "prompt=Keep the person unchanged and change the window lights to warm golden sunset light." \
  -F "image=@source.png" \
  -F "mask=@mask.png" \
  -F "size=1024x1024" \
  -F "quality=low" \
  -F "n=1" \
  -F "response_format=url"
```

编辑任务支持的常用字段如下：

| 字段 | 类型 | JSON | multipart | 说明 |
| --- | --- | --- | --- | --- |
| `model` | string | 可选 | 可选 | 推荐 `gpt-image-2` |
| `prompt` | string | 推荐 | 推荐 | 编辑说明 |
| `images[].image_url` | string | 必填 | 不适用 | JSON 源图 URL |
| `image` | file | 不适用 | 必填 | multipart 源图 |
| `mask.image_url` | string | 可选 | 不适用 | JSON 遮罩 URL |
| `mask` | file | 不适用 | 可选 | multipart 遮罩图 |
| `size` | string | 可选 | 可选 | 例如 `1024x1024`、`1536x1024` |
| `quality` | string | 可选 | 可选 | `low`、`medium`、`high` 或 `auto` |
| `n` | integer | 可选 | 可选 | 推荐 `1` |
| `response_format` | string | 可选 | 可选 | 推荐 `url` |
| `input_fidelity` | string | 可选 | 可选 | 按模型能力使用 |
| `output_format` | string | 可选 | 可选 | 例如 `png`、`jpeg`、`webp` |
| `output_compression` | integer | 可选 | 可选 | 输出压缩质量 |
| `background` / `moderation` / `style` / `partial_images` | mixed | 可选 | 可选 | 按模型能力使用 |

异步任务不支持 `stream=true`；`file_id` 也不支持，请改用 `image_url` 或直接上传 `image` 文件。Windows `curl.exe` 上传本地文件时，应确保完整的 `image=@<path>` 参数作为一个参数传递；路径含空格时建议使用引号或临时复制到无空格路径。

### 编辑任务成功响应

响应格式与异步生图相同：读取 `task_id` 或 `id`，再查询 `/v1/images/tasks/{task_id}`。完成后从 `result.data[0].url` 或 `image_url` 取结果 URL。

## 3. 查询任务状态

### 请求

```http
GET /v1/images/tasks/{task_id} HTTP/1.1
Host: api.xzgc.asia
Authorization: Bearer <SUB2API_API_KEY>
```

建议每 3 至 5 秒查询一次，总等待时间设置为 10 分钟左右。

### 处理中

```json
{
  "id": "imgtask_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
  "task_id": "imgtask_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
  "object": "image.generation.task",
  "status": "processing",
  "http_status": 202,
  "created_at": 1786966022,
  "expires_at": 1787052460
}
```

客户端必须以 `status` 为准。`202` 和 `processing` 只表示任务已受理，不代表图片已经生成成功。

### 已完成

```json
{
  "id": "imgtask_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
  "task_id": "imgtask_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
  "object": "image.generation.task",
  "status": "completed",
  "http_status": 200,
  "image_url": "https://img.chenshuapi.com/images/<file>.png",
  "result": {
    "created": 1786966057,
    "data": [
      {
        "url": "https://img.chenshuapi.com/images/<file>.png"
      }
    ],
    "usage": {
      "input_tokens": 9693,
      "output_tokens": 1536,
      "total_tokens": 11229
    }
  },
  "created_at": 1786966022,
  "completed_at": 1786966060,
  "expires_at": 1787052460
}
```

结果 URL 建议按以下顺序读取：

```text
image_url = response.result.data[0].url ?? response.image_url
```

拿到 URL 后应立即下载并验证：HTTP 状态、响应字节数、图片能否解码、实际像素尺寸。CDN 图片下载不需要再次携带 API Key。

### 失败

```json
{
  "id": "imgtask_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
  "task_id": "imgtask_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
  "object": "image.generation.task",
  "status": "failed",
  "error": {
    "type": "api_error",
    "message": "failure description"
  },
  "created_at": 1786966022,
  "completed_at": 1786966060,
  "expires_at": 1787052460
}
```

已观察到的状态为 `processing`、`completed`、`failed`。客户端还应对未知终态做保护：达到总超时后停止轮询并保存 `task_id`，不要自动重新 POST。

## 4. PowerShell 完整示例

先在当前终端设置密钥：

```powershell
$env:SUB2API_API_KEY = "在本机设置的新密钥"
```

执行一次创建并轮询：

```powershell
$baseUrl = "https://api.xzgc.asia"
$headers = @{
    Authorization  = "Bearer $env:SUB2API_API_KEY"
    "X-Request-ID" = "async-image-$([guid]::NewGuid())"
}

$body = @{
    model           = "gpt-image-2"
    prompt          = "A tiny glass greenhouse on a snowy mountain at sunrise, realistic product photography"
    size            = "1536x1024"
    quality         = "low"
    n               = 1
    response_format = "url"
} | ConvertTo-Json

$task = Invoke-RestMethod `
    -Method Post `
    -Uri "$baseUrl/v1/images/generations/async" `
    -Headers $headers `
    -ContentType "application/json" `
    -Body $body

$taskId = if ($task.task_id) { $task.task_id } else { $task.id }
if (-not $taskId) { throw "异步接口未返回 task_id 或 id" }

$deadline = (Get-Date).AddMinutes(10)
do {
    Start-Sleep -Seconds 4
    $result = Invoke-RestMethod `
        -Method Get `
        -Uri "$baseUrl/v1/images/tasks/$taskId" `
        -Headers @{ Authorization = "Bearer $env:SUB2API_API_KEY" }

    Write-Host "task=$taskId status=$($result.status)"
    if ($result.status -eq "completed") { break }
    if ($result.status -eq "failed") {
        throw "生图失败: $($result.error.message)"
    }
} while ((Get-Date) -lt $deadline)

if ($result.status -ne "completed") {
    throw "轮询超时；保留 task_id=$taskId，禁止自动重新提交"
}

$imageUrl = if ($result.result.data[0].url) {
    $result.result.data[0].url
} else {
    $result.image_url
}

Invoke-WebRequest -Uri $imageUrl -OutFile ".\async-image-$taskId.png"
```

如果当前机器访问 CDN 时优先选择了不可用的 IPv6，可将最后一步改成：

```powershell
curl.exe -4 -fL $imageUrl -o ".\async-image-$taskId.png"
```

## 5. curl 示例

```bash
export SUB2API_API_KEY='在本机设置的新密钥'
export BASE_URL='https://api.xzgc.asia'

curl -sS -X POST "$BASE_URL/v1/images/generations/async" \
  -H "Authorization: Bearer $SUB2API_API_KEY" \
  -H "Content-Type: application/json" \
  -H "X-Request-ID: async-image-$(date +%s)" \
  -d '{
    "model": "gpt-image-2",
    "prompt": "A tiny glass greenhouse on a snowy mountain at sunrise, realistic product photography",
    "size": "1536x1024",
    "quality": "low",
    "n": 1,
    "response_format": "url"
  }'
```

把返回的 `task_id` 或 `id` 填入：

```bash
curl -sS "$BASE_URL/v1/images/tasks/<task_id>" \
  -H "Authorization: Bearer $SUB2API_API_KEY"
```

任务完成后下载：

```bash
curl -4 -fL '<image_url>' -o result.png
```

## 6. Python 示例

依赖：`pip install requests`

```python
import os
import time
import uuid

import requests

base_url = "https://api.xzgc.asia"
api_key = os.environ["SUB2API_API_KEY"]
headers = {
    "Authorization": f"Bearer {api_key}",
    "Content-Type": "application/json",
    "X-Request-ID": f"async-image-{uuid.uuid4()}",
}
payload = {
    "model": "gpt-image-2",
    "prompt": "A tiny glass greenhouse on a snowy mountain at sunrise, realistic product photography",
    "size": "1536x1024",
    "quality": "low",
    "n": 1,
    "response_format": "url",
}

created = requests.post(
    f"{base_url}/v1/images/generations/async",
    headers=headers,
    json=payload,
    timeout=30,
)
created.raise_for_status()
task = created.json()
task_id = task.get("task_id") or task.get("id")
if not task_id:
    raise RuntimeError(f"missing task id: {task}")

deadline = time.monotonic() + 600
while time.monotonic() < deadline:
    time.sleep(4)
    response = requests.get(
        f"{base_url}/v1/images/tasks/{task_id}",
        headers={"Authorization": f"Bearer {api_key}"},
        timeout=15,
    )
    response.raise_for_status()
    task = response.json()
    status = task.get("status")
    print(f"task={task_id} status={status}")

    if status == "completed":
        break
    if status == "failed":
        raise RuntimeError(task.get("error") or task)
else:
    raise TimeoutError(
        f"polling timed out; keep task_id={task_id} and do not resubmit"
    )

data = (task.get("result") or {}).get("data") or []
image_url = data[0].get("url") if data else task.get("image_url")
if not image_url:
    raise RuntimeError(f"completed task has no image URL: {task}")

image = requests.get(image_url, timeout=60)
image.raise_for_status()
if not image.content:
    raise RuntimeError("downloaded image is empty")

output_path = f"async-image-{task_id}.png"
with open(output_path, "wb") as file:
    file.write(image.content)
print(output_path)
```

## 7. 常见 HTTP 错误

| 状态码 | 常见原因 | 处理方式 |
| --- | --- | --- |
| `400` | JSON、字段类型、模型或尺寸无效 | 修正请求参数后再提交 |
| `401` | API Key 缺失、无效或已禁用 | 检查本机环境变量和 Key 状态 |
| `403` | Key 所属分组未允许生图 | 检查分组的图片生成权限 |
| `402` / `429` | 余额、套餐、并发或频率限制 | 查看错误正文和账户状态，按 `Retry-After` 等待 |
| `500` / `502` / `504` | 上游、对象存储或网关错误 | 若已有 `task_id`，只查询原任务，不要立即重提 |

错误正文通常为：

```json
{
  "error": {
    "type": "invalid_request_error",
    "message": "error description"
  }
}
```

## 8. 计费与防重复规则

1. `202 processing` 只表示已受理，不等于生成成功。
2. 创建任务只能 POST 一次；网络断开或轮询超时不允许自动重新 POST。
3. 已拿到 `task_id` 后，只轮询该任务，直到 `completed`、`failed` 或达到人工处理超时。
4. 图片 URL 下载失败时，应重试同一个 URL，不能重新生图。
5. `result.usage` 是模型 token 统计，不是最终货币费用；费用以 Sub2API 用量记录为准。
6. 当前生图专用分组已验证单张价格为 `0.03`，但生产价格以后台实时配置为准。
7. 当前实测 `1024x1024` 记为 `1K`，`1536x1024` 记为 `2K`。

## 9. 交给其他 Codex 的任务说明

可以将下面这段直接交给另一个 Codex：

```text
使用环境变量 SUB2API_API_KEY 调用 https://api.xzgc.asia 的异步生图服务。
只允许 POST 一次 /v1/images/generations/async，参数使用 gpt-image-2、n=1、response_format=url，并设置唯一 X-Request-ID。
从响应的 task_id 或 id 取得任务 ID，每 4 秒 GET /v1/images/tasks/{task_id}。
只有 status=completed 才下载 result.data[0].url 或 image_url；status=failed 时输出 error；10 分钟超时后保留 task_id 并停止，禁止重新 POST。
下载后检查文件非空、图片可解码和实际尺寸。不要输出、记录或提交 API Key。
完整接口文档位于 docs/ASYNC_IMAGE_GENERATION_API.md。
```

## 10. 已完成的生产验收

- `gpt-image-2 / 1024x1024 / low / n=1`：`processing -> completed`
- `gpt-image-2 / 1536x1024 / low / n=1`：`processing -> completed`
- 第二次任务约 38 秒完成，结果 PNG 实际尺寸为 `1536x1024`
- 对象存储、`img.chenshuapi.com` CDN 和用量计费记录均已核对
- 香港 VPS 到 CDN 的 IPv6 当前不可用，服务端或运维脚本下载时建议使用 `curl -4`
- 异步图片编辑真实验收：multipart 上传本地 PNG，`gpt-image-2 / 1024x1024 / low / n=1 / response_format=url`，HTTP `202` 后由 `processing` 转为 `completed`
- 本次编辑结果下载 HTTP `200`，文件 `498618` 字节、可解码，实际尺寸 `1254x1254`；提示词要求将窗外夜景改为暖金色日落，视觉结果已确认生效
