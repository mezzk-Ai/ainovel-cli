package tui

import "testing"

// TestParseImportArgsGuide 守护 --guide 解析：自然语言指导可含空格（其后 token 全部并入），
// 可与其它选项组合（置于最后），空内容报错。
func TestParseImportArgsGuide(t *testing.T) {
	opts, err := parseImportArgs([]string{"--guide=幕间·X", "也是", "独立章节"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Guidance != "幕间·X 也是 独立章节" {
		t.Fatalf("含空格指导应整体并入，得 %q", opts.Guidance)
	}
	opts, err = parseImportArgs([]string{"book.txt", "--yes", "--guide=序章并入第一章"})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.AutoConfirm || opts.SourcePath != "book.txt" || opts.Guidance != "序章并入第一章" {
		t.Fatalf("与其它选项组合解析不符：%+v", opts)
	}
	if _, err := parseImportArgs([]string{"--guide="}); err == nil {
		t.Fatal("空 --guide 应报错")
	}
}
