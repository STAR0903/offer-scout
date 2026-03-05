package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func main() {
	// 强制所有日志输出到 Stderr，防止干扰 MCP 的 Stdout 通信
	log.SetOutput(os.Stderr)
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)

	// 创建 MCP Server
	s := server.NewMCPServer(
		"OfferScout",
		"1.0.0",
		server.WithToolCapabilities(true),
		server.WithRecovery(),
	)

	// 注册牛客网专用爬取工具
	scrapePostsTool := mcp.NewTool("scrape_nowcoder_posts",
		mcp.WithDescription("专门爬取牛客网(nowcoder.com)的帖子列表，自动翻页获取所有帖子的链接 and 内容。"),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description("牛客网目标网址，必须是 nowcoder.com 域名下的链接"),
		),
		mcp.WithNumber("start_page",
			mcp.Description("起始页码，默认为 1。分页逻辑为连续抓取 [start_page, start_page + max_pages - 1] 范围的页面。若本次抓取了 1-2 页，下次起始页应设为 3"),
		),
		mcp.WithNumber("max_pages",
			mcp.Description("最大翻页数，默认为 3。例如 start_page=1, max_pages=2 则会抓取第 1 和第 2 页"),
		),
		mcp.WithNumber("max_posts",
			mcp.Description("最大获取帖子数，默认为 40"),
		),
	)

	s.AddTool(scrapePostsTool, handleScrapePosts)

	// 使用 stdio 传输方式启动服务
	log.Println("OfferScout MCP Server 启动中...")
	if err := server.ServeStdio(s); err != nil {
		log.Printf("服务器运行致命错误: %v\n", err)
	}
}

// handleScrapePosts 处理爬取牛客网帖子的工具调用
func handleScrapePosts(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// 获取必填参数
	url, err := request.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("缺少必要参数 url: " + err.Error()), nil
	}

	// 校验域名，确保只针对牛客网
	if !strings.Contains(url, "nowcoder.com") {
		return mcp.NewToolResultError("该工具仅支持牛客网(nowcoder.com)的链接，请提供牛客网的 URL"), nil
	}

	// 获取可选参数（使用与 scraper 一致的默认值）
	startPage := 1
	if sp, err := request.RequireFloat("start_page"); err == nil {
		startPage = int(sp)
	}

	maxPages := 3
	if mp, err := request.RequireFloat("max_pages"); err == nil {
		maxPages = int(mp)
	}

	maxPosts := 40
	if mp, err := request.RequireFloat("max_posts"); err == nil {
		maxPosts = int(mp)
	}

	// 执行爬取
	sc := NewScraper(startPage, maxPages, maxPosts)
	posts, err := sc.ScrapeAll(ctx, url)
	if err != nil {
		return mcp.NewToolResultError("爬取失败: " + err.Error()), nil
	}

	// 格式化结果为 JSON
	result, err := json.MarshalIndent(posts, "", "  ")
	if err != nil {
		return mcp.NewToolResultError("序列化结果失败: " + err.Error()), nil
	}

	return mcp.NewToolResultText(string(result)), nil
}
