package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"bytes"
	"io"

	"github.com/PuerkitoBio/goquery"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"golang.org/x/sync/errgroup"
)

// ============================================================
// 数据结构
// ============================================================

// Post 表示一个帖子
type Post struct {
	Title   string `json:"title"`   // 帖子标题
	Link    string `json:"link"`    // 帖子链接
	Content string `json:"content"` // 帖子内容
}

// Scraper 牛客网爬虫核心 (基于 chromedp)
type Scraper struct {
	StartPage int // 作为兼容保留（实际上主要用作第一次打开的 page=xx）
	MaxPages  int // 预计向下滚动的次数或模拟翻页数
	MaxPosts  int // 最大帖子数
}

// ============================================================
// 构造与入口
// ============================================================

// NewScraper 创建爬虫实例
func NewScraper(startPage, maxPages, maxPosts int) *Scraper {
	if startPage <= 0 {
		startPage = 1
	}
	if maxPages <= 0 {
		maxPages = 3
	}
	if maxPosts <= 0 {
		maxPosts = 40
	}
	return &Scraper{
		StartPage: startPage,
		MaxPages:  maxPages,
		MaxPosts:  maxPosts,
	}
}

// ScrapeAll 爬取所有帖子：通过基于 Chrome 浏览器的 CDP 抓取列表并提取详情
func (s *Scraper) ScrapeAll(ctx context.Context, targetURL string) ([]Post, error) {
	// 创建 CDP 运行环境 (Headless Chrome)
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.DisableGPU,
		chromedp.NoDefaultBrowserCheck,
		chromedp.Flag("headless", true),                        // 无头模式运行，不弹窗
		chromedp.Flag("blink-settings", "imagesEnabled=false"), // 不加载图片，加速
	)

	allocCtx, cancel := chromedp.NewExecAllocator(ctx, opts...)
	defer cancel()

	// 创建带超时的顶层 context (比如整次爬虫最多允许 5 分钟)
	taskCtx, taskCancel := context.WithTimeout(allocCtx, 5*time.Minute)
	defer taskCancel()

	// 创建浏览器实例的 Context
	chromeCtx, cancelChrome := chromedp.NewContext(taskCtx, chromedp.WithLogf(log.Printf))
	defer cancelChrome()

	// 第一步：获取列表页的所有帖子链接
	links, err := s.scrapePostLinksWithCDP(chromeCtx, targetURL)
	if err != nil {
		// 即使失败，也得尝试关闭浏览器释放资源
		cancelChrome()
		return nil, fmt.Errorf("获取帖子链接失败: %w", err)
	}

	// 核心优化：获取完链接后立即主动关闭浏览器进程，后续详情提取改用 Fast (HTTP) 模式
	cancelChrome()

	if len(links) == 0 {
		return nil, fmt.Errorf("未找到任何帖子链接")
	}

	// 限制帖子数量
	if len(links) > s.MaxPosts {
		links = links[:s.MaxPosts]
	}

	log.Printf("成功提取到 %d 个帖子链接，开始逐个获取正文...", len(links))

	// 第二步：并行获取帖子内容
	var posts []Post
	results := make(chan Post, len(links))

	// 使用 errgroup 控制并发数
	g, gCtx := errgroup.WithContext(ctx)
	// 限制并发链接数，避免被封禁，默认设为 5
	sem := make(chan struct{}, 5)

	for _, link := range links {
		link := link // 闭包陷阱
		g.Go(func() error {
			sem <- struct{}{}
			defer func() { <-sem }()

			log.Printf("正在并发获取帖子内容: %s", link.Title)
			content, err := s.scrapePostContentFast(gCtx, link.Link)
			if err != nil {
				log.Printf("获取帖子内容失败 [%s]: %v", link.Link, err)
				content = "获取内容失败: " + err.Error()
			}

			results <- Post{
				Title:   link.Title,
				Link:    link.Link,
				Content: content,
			}
			return nil
		})
	}

	// 等待所有抓取完成
	go func() {
		_ = g.Wait()
		close(results)
	}()

	// 收集结果
	for p := range results {
		posts = append(posts, p)
	}

	return posts, nil
}

// ============================================================
// 列表页解析 (基于 Chrome 持续向下滚动)
// ============================================================

func (s *Scraper) scrapePostLinksWithCDP(ctx context.Context, targetURL string) ([]Post, error) {
	parsedURL, _ := url.Parse(targetURL)
	baseURL := fmt.Sprintf("%s://%s", parsedURL.Scheme, parsedURL.Host)
	query := parsedURL.Query().Get("query")

	log.Printf("正在打开列表页并获取 Cookie: %s", targetURL)
	var cookies []*network.Cookie
	err := chromedp.Run(ctx,
		chromedp.Navigate(targetURL),
		chromedp.WaitReady(`body`, chromedp.ByQuery),
		chromedp.Sleep(2*time.Second),
		chromedp.ActionFunc(func(ctx context.Context) error {
			var err error
			cookies, err = network.GetCookies().Do(ctx)
			return err
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("初始化页面或获取 Cookie 失败: %w", err)
	}

	cookieStr := ""
	for _, c := range cookies {
		cookieStr += fmt.Sprintf("%s=%s; ", c.Name, c.Value)
	}

	var allLinks []Post
	seen := make(map[string]bool)

	// 通用的解析函数：从 HTML 中提取链接 (用于首页)
	parseFromHTML := func() {
		var currentHTML string
		_ = chromedp.Run(ctx, chromedp.OuterHTML("html", &currentHTML))
		doc, _ := goquery.NewDocumentFromReader(strings.NewReader(currentHTML))
		selectors := []string{"a[href*='/feed/main/detail/']", "a[href*='/discuss/']"}

		doc.Find(strings.Join(selectors, ", ")).Each(func(i int, sel *goquery.Selection) {
			href, exists := sel.Attr("href")
			if !exists {
				return
			}
			fullURL := normalizeURL(href, baseURL)
			cleanURL := stripQueryParams(fullURL)
			if seen[cleanURL] {
				return
			}
			title := strings.TrimSpace(sel.Text())
			if title == "" || isNonPostURL(fullURL) {
				return
			}
			seen[cleanURL] = true
			allLinks = append(allLinks, Post{Title: title, Link: fullURL})
		})
	}

	// 1. 处理起始页 (如果是第一页，直接从当前浏览器的 HTML 解析，如果是后续页，由 API 处理)
	if s.StartPage == 1 {
		log.Printf("解析第 1 页 (HTML 模式)")
		parseFromHTML()
	}

	// 2. 循环处理所有需要的页面
	actualStart := s.StartPage
	if actualStart == 1 {
		actualStart = 2 // 如果从 1 开始且已经解析了 HTML，则 API 从 2 开始
	}

	pagesToFetchByAPI := s.MaxPages
	if s.StartPage == 1 {
		pagesToFetchByAPI = s.MaxPages - 1
	}

	for p := 0; p < pagesToFetchByAPI; p++ {
		currentPage := actualStart + p
		log.Printf("正在通过 API 抓取第 %d 页 (进度 %d/%d)", currentPage, p+1, pagesToFetchByAPI)

		apiLinks, err := s.fetchPageLinksViaAPI(ctx, query, currentPage, cookieStr)
		if err != nil {
			log.Printf("API 抓取第 %d 页失败: %v，停止后续翻页", currentPage, err)
			break
		}

		newCount := 0
		for _, link := range apiLinks {
			cleanURL := stripQueryParams(link.Link)
			if !seen[cleanURL] {
				seen[cleanURL] = true
				allLinks = append(allLinks, link)
				newCount++
			}
		}
		log.Printf("第 %d 页通过 API 获取到 %d 条新链接", currentPage, newCount)
		// 适当延迟，避免 API 频率限制
		time.Sleep(1 * time.Second)
	}

	return allLinks, nil
}

// fetchPageLinksViaAPI 直接调用牛客网搜索接口获取数据 (AJAX 模式)
func (s *Scraper) fetchPageLinksViaAPI(ctx context.Context, query string, page int, cookieStr string) ([]Post, error) {
	apiURL := "https://gw-c.nowcoder.com/api/sparta/pc/search"
	payload := map[string]interface{}{
		"type":  "all",
		"query": query,
		"page":  page,
		"tag":   []string{},
		"order": "",
		"gioParams": map[string]string{
			"searchFrom_var":  "搜索页输入框",
			"searchEnter_var": "主站",
		},
	}
	jsonData, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Referer", "https://www.nowcoder.com/")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Cookie", cookieStr)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API 状态码异常: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var res struct {
		Data struct {
			Records []struct {
				Data map[string]interface{} `json:"data"`
			} `json:"records"`
		} `json:"data"`
	}

	if err := json.Unmarshal(body, &res); err != nil {
		return nil, err
	}

	var posts []Post
	for _, rec := range res.Data.Records {
		var title, uuid, idStr string
		var contentType float64

		if ct, ok := rec.Data["contentType"].(float64); ok {
			contentType = ct
		}

		// 遍历 data 下的所有字段（例如 momentData, postData, contentData 等）
		for _, val := range rec.Data {
			if m, ok := val.(map[string]interface{}); ok {
				if t, ok := m["title"].(string); ok && t != "" {
					title = t
				}
				if u, ok := m["uuid"].(string); ok && u != "" {
					uuid = u
				}
				if idVal, ok := m["id"]; ok {
					idStr = fmt.Sprintf("%v", idVal)
				}
			}
		}

		if title != "" {
			var link string
			// 根据 contentType 和数据是否存在生成链接
			if contentType == 250 && idStr != "" {
				link = fmt.Sprintf("https://www.nowcoder.com/discuss/%s?sourceSSR=search", idStr)
			} else if uuid != "" {
				link = fmt.Sprintf("https://www.nowcoder.com/feed/main/detail/%s?sourceSSR=search", uuid)
			} else if idStr != "" {
				link = fmt.Sprintf("https://www.nowcoder.com/discuss/%s?sourceSSR=search", idStr)
			} else {
				continue
			}
			posts = append(posts, Post{
				Title: title,
				Link:  link,
			})
		}
	}

	return posts, nil
}

// ============================================================
func (s *Scraper) scrapePostContentFast(ctx context.Context, postURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", postURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return "", err
	}

	html, _ := doc.Html()

	// 从 URL 中提取 ID (通常是 32 位十六进制或纯数字)
	targetID := ""
	idRe := regexp.MustCompile(`/(detail|discuss)/([a-z0-9]+)`)
	idMatches := idRe.FindStringSubmatch(postURL)
	if len(idMatches) >= 3 {
		targetID = idMatches[2]
	}

	// 核心修复：寻找 window.__INITIAL_STATE__ 并根据 ID 精准匹配
	// 核心修复：更健壮地寻找 window.__INITIAL_STATE__
	// 牛客的这个变量可能非常巨大（包含几万行的 JSON），正则非贪婪匹配容易在中间的字符串处中断
	re := regexp.MustCompile(`window\.__INITIAL_STATE__\s*=\s*(\{.+?\});\s*<`)
	if !re.MatchString(html) {
		// 备选方案：匹配到行尾
		re = regexp.MustCompile(`window\.__INITIAL_STATE__\s*=\s*(\{.*\});`)
	}

	matches := re.FindStringSubmatch(html)
	if len(matches) >= 2 {
		var data interface{}
		if err := json.Unmarshal([]byte(matches[1]), &data); err == nil {
			// 先检查是否是“内容不存在”等错误页面
			if errMsg := checkErrorMessage(data); errMsg != "" {
				return errMsg, nil
			}

			bestContent := ""
			// 优先尝试根据 ID 精准定位
			if targetID != "" {
				bestContent = findContentByTargetID(data, targetID)
			}

			// 如果 ID 匹配失败，降级为搜索全局最长 content (但排除推荐列表等干扰)
			if bestContent == "" {
				findAllContentsWeighted(data, "content", &bestContent)
			}

			// 放宽长度限制，很多帖子只有几句话或全是图片
			if len([]rune(bestContent)) >= 5 {
				return truncateContent(cleanText(stripHTML(bestContent)), 50000), nil
			} else if bestContent != "" {
				return truncateContent(cleanText(stripHTML(bestContent)), 50000), nil
			}
		}
	}

	// 回退方案保持不变...
	metaContent := extractMetaContent(doc)
	removeNoiseElements(doc)
	contentSelectors := []string{".feed-content-text", "[class*='feed-content']", ".post-topic-main", ".nc-post-content"}
	for _, selector := range contentSelectors {
		selection := doc.Find(selector)
		if selection.Length() > 0 {
			text := cleanText(selection.Text())
			if len([]rune(text)) > 30 && !isContentNoisy(text) {
				return truncateContent(text, 50000), nil
			}
		}
	}
	if metaContent != "" {
		return metaContent, nil
	}
	return truncateContent(cleanText(doc.Find("body").Text()), 30000), nil
}

// findContentByTargetID 在 JSON 中寻找 ID 匹配当前帖子 URL 的对象，并提取其 content
func findContentByTargetID(v interface{}, targetID string) string {
	best := ""
	var traverse func(val interface{})
	traverse = func(val interface{}) {
		switch x := val.(type) {
		case map[string]interface{}:
			matched := false
			if id, ok := x["contentId"].(string); ok && id == targetID {
				matched = true
			} else if id, ok := x["uuid"].(string); ok && id == targetID {
				matched = true
			} else if id, ok := x["id"]; ok && fmt.Sprintf("%v", id) == targetID {
				matched = true
			}

			if matched {
				// 尝试多个可能的正文字段
				for _, key := range []string{"content", "postContent", "detailContent"} {
					if c, ok := x[key].(string); ok {
						if len([]rune(c)) > len([]rune(best)) {
							best = c
						}
					}
				}
				// 递归查找当前对象下的深层 content（复用过滤逻辑，抛弃评论等）
				var localBest string
				findAllContentsWeighted(x, "content", &localBest)
				if len([]rune(localBest)) > len([]rune(best)) {
					best = localBest
				}
			}
			// 继续往下级节点遍历
			for _, v := range x {
				traverse(v)
			}
		case []interface{}:
			for _, v := range x {
				traverse(v)
			}
		}
	}
	traverse(v)
	return best
}

// checkErrorMessage 检查 JSON 数据中是否有明确的错误提示（如帖子已被删除）
func checkErrorMessage(v interface{}) string {
	msg := ""
	var traverse func(val interface{})
	traverse = func(val interface{}) {
		if msg != "" {
			return
		}
		switch x := val.(type) {
		case map[string]interface{}:
			// 检查形如 {"showMessage": {"message": "内容不存在!"}} 的结构
			if sm, ok := x["showMessage"].(map[string]interface{}); ok {
				if m, ok := sm["message"].(string); ok && m != "" {
					msg = m
					return
				}
			}
			for _, v := range x {
				traverse(v)
			}
		case []interface{}:
			for _, v := range x {
				traverse(v)
			}
		}
	}
	traverse(v)
	return msg
}

// findAllContentsWeighted 递归扫描所有字段，并将最长的目标字段保存在 bestContent 中（避开已知噪点路径）
func findAllContentsWeighted(v interface{}, target string, bestContent *string) {
	switch val := v.(type) {
	case map[string]interface{}:
		if c, ok := val[target]; ok {
			if strC, ok := c.(string); ok {
				if len([]rune(strC)) > len([]rune(*bestContent)) {
					*bestContent = strC
				}
			}
		}
		for k, v := range val {
			// 避开推荐列表、热榜、评论、相关内容等干扰项
			if k == "hotList" || k == "recommendList" || k == "adList" || k == "bannerList" ||
				k == "commentList" || k == "commentListFirst" || k == "comments" || k == "commentExposure" ||
				k == "relateContents" || k == "relevantPosts" || k == "similarPosts" || k == "subjectData" ||
				k == "relatedDiscuss" || k == "voteData" || k == "answerList" || k == "hotSubjectList" || k == "similarRecommend" {
				continue
			}
			findAllContentsWeighted(v, target, bestContent)
		}
	case []interface{}:
		for _, v := range val {
			findAllContentsWeighted(v, target, bestContent)
		}
	}
}

// ============================================================
// 工具函数 (重用之前的逻辑)
// ============================================================

func extractMetaContent(doc *goquery.Document) string {
	var content string
	if desc, exists := doc.Find("meta[property='og:description']").Attr("content"); exists && len([]rune(desc)) > 20 {
		content = cleanText(desc)
	} else if desc, exists := doc.Find("meta[name='description']").Attr("content"); exists && len([]rune(desc)) > 20 {
		content = cleanText(desc)
	}

	if content != "" {
		content = strings.ReplaceAll(content, "_牛客网_牛客在手,offer不愁", "")
		content = strings.TrimSpace(content)
		if strings.Contains(content, "求职之前，先上牛客") {
			return ""
		}
		return content
	}
	return ""
}

func removeNoiseElements(doc *goquery.Document) {
	noiseSelectors := []string{
		"[class*='comment']", "[class*='Comment']", "#comments", ".reply-list", "[class*='reply']",
		"[class*='recommend']", "[class*='Recommend']", "[class*='related']", "[class*='Related']", "[class*='suggest']",
		"[class*='sidebar']", "[class*='Sidebar']", "[class*='side-bar']", "aside",
		"[class*='hot']", "[class*='Hot']", "[class*='rank']", "[class*='Rank']",
		"header", "nav", "footer", "[class*='header']", "[class*='Header']", "[class*='footer']", "[class*='Footer']", "[class*='nav']", "[class*='Nav']",
		"[class*='ad']", "[class*='Ad']", "[class*='banner']", "[class*='Banner']", "[class*='modal']", "[class*='Modal']", "[class*='popup']",
		"[class*='quick']", "[class*='Quick']", "[class*='action-bar']", "[class*='toolbar']", "[class*='share']", "[class*='Share']", "[class*='follow']", "[class*='Follow']",
	}
	for _, selector := range noiseSelectors {
		doc.Find(selector).Remove()
	}
}

func isContentNoisy(text string) bool {
	noiseKeywords := []string{
		"全部评论", "推荐最新", "相关推荐", "全站热榜",
		"创作者周榜", "正在热议", "次浏览", "人参与",
		"点赞", "收藏", "分享", "评论", "查看真题", "抢首评", "蹲蹲面经", "接好运", "发布于", "查看解析",
	}
	noiseCount := 0
	for _, keyword := range noiseKeywords {
		noiseCount += strings.Count(text, keyword)
	}
	textLen := len([]rune(text))
	if textLen == 0 {
		return true
	}
	return (float64(noiseCount) / (float64(textLen) / 1000.0)) > 5.0
}

func truncateContent(text string, maxRunes int) string {
	runes := []rune(text)
	if len(runes) > maxRunes {
		return string(runes[:maxRunes]) + "..."
	}
	return text
}

func cleanText(text string) string {
	lines := strings.Split(text, "\n")
	var cleanedLines []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			cleanedLines = append(cleanedLines, trimmed)
		}
	}
	return strings.Join(cleanedLines, "\n")
}

// stripHTML 去除字符串中的 HTML 标签
func stripHTML(s string) string {
	// 简单的正则去除，如果需要更复杂的可以引入专门库，但这里通常足够
	re := regexp.MustCompile(`<[^>]*>`)
	return re.ReplaceAllString(s, "")
}

func buildPageURL(baseURL string, page int) string {
	if page <= 1 {
		return baseURL
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return baseURL
	}
	q := u.Query()
	q.Set("page", fmt.Sprintf("%d", page))
	u.RawQuery = q.Encode()
	return u.String()
}

func normalizeURL(href, baseURL string) string {
	if strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://") {
		return href
	}
	if strings.HasPrefix(href, "//") {
		return "https:" + href
	}
	if strings.HasPrefix(href, "/") {
		return baseURL + href
	}
	return baseURL + "/" + href
}

func stripQueryParams(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func isNonPostURL(fullURL string) bool {
	nonPostPatterns := []string{
		"/users/", "/login", "/register", "/exam/", "/courses", "/jobs/", "/interview/ai/", "/simple/",
	}
	for _, pattern := range nonPostPatterns {
		if strings.Contains(fullURL, pattern) {
			return true
		}
	}
	return false
}
