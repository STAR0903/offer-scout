package main

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

func TestScrape(t *testing.T) {
	t.Log("开始测试抓取牛客网 go面经")
	// 抓取第1到第2页，最多获取80条帖子
	sc := NewScraper(1, 2, 80)
	posts, err := sc.ScrapeAll(context.Background(), "https://www.nowcoder.com/search/all?query=go%E9%9D%A2%E7%BB%8F")
	if err != nil {
		t.Fatalf("抓取失败: %v", err)
	}

	data, err := json.MarshalIndent(posts, "", "  ")
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}

	err = os.WriteFile("test_output.json", data, 0644)
	if err != nil {
		t.Fatalf("写入文件失败: %v", err)
	}

	t.Logf("成功抓取 %d 条帖子，已保存到 test_output.json", len(posts))
}
