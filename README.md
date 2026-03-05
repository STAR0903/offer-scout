# OfferScout

牛客网面经爬虫 MCP Server —— 专门用于爬取牛客网 (nowcoder.com) 面经帖子内容的工具。

## 功能

- **自动翻页**：支持指定起始页码和翻页数量。
- **高效去噪**：自动清洗评论、广告、推荐等干扰内容。
- **正文提取**：针对牛客网多种页面布局优化，确保提取完整面经。

## 安装

**前提条件**：
- Go 1.21+ 环境
- 系统中须安装 Google Chrome、Microsoft Edge 或 Chromium (本程序会自动调度无头浏览器进行页面动态滚动和正文数据爬取)，且登录牛客网。

```bash
git clone https://github.com/STAR0903/offer-scout.git
cd offer-scout
go build -o offer-scout.exe .
```

## MCP 配置

将生成的二进制文件路径添加到你的 MCP 客户端配置中（例如 Claude Desktop 或 Cursor）：

```json
{
  "mcpServers": {
    "nowcoder-scout": {
      "command": "C:\\path\\to\\your\\offer-scout.exe"
    }
  }
}
```
> **注意**：请将 `C:\\path\\to\\your\\offer-scout.exe` 替换为你实际编译出的文件绝对路径。

## 工具说明

### `scrape_nowcoder_posts`

**输入参数：**

| 参数 | 类型 | 说明 | 默认值 |
| :--- | :--- | :--- | :--- |
| `url` | string | **(必填)** 牛客网目标网址（搜索页或讨论区） | - |
| `start_page` | number | 起始页码 | 1 | 抓取范围为 [start_page, start_page + max_pages - 1] |
| `max_pages` | number | 最大翻页数 | 3 | 例如 start_page=1, max_pages=2 会抓取 1 和 2 页 |
| `max_posts` | number | 最大提取帖子数 | 40 |

**调用示例：**

```json
{
  "url": "https://www.nowcoder.com/search/all?query=go面经",
  "max_pages": 1
}
```

