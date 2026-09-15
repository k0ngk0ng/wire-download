package engine

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestAMuleExecutorUsesUTF8Locale(t *testing.T) {
	if os.Getenv("WIRE_TEST_AMULE_LOCALE_CHILD") == "1" {
		locale := os.Getenv("LC_ALL")
		if !strings.Contains(strings.ToUpper(locale), "UTF-8") || locale != os.Getenv("LANG") {
			os.Exit(2)
		}
		fmt.Println(" > 0123456789abcdef0123456789abcdef 测试文件.iso")
		fmt.Println(" > [25.0%] 1/2 - Downloading - 001.part.met - Auto")
		os.Exit(0)
	}
	t.Setenv("WIRE_TEST_AMULE_LOCALE_CHILD", "1")
	t.Setenv("LC_ALL", "C")
	t.Setenv("LANG", "C")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	out, err := defaultAMuleCommandExecutor(context.Background(), binary, "-test.run=^TestAMuleExecutorUsesUTF8Locale$")
	if err != nil {
		t.Fatal(err)
	}
	items := parseAMuleDownloads(out)
	if len(items) != 1 || items[0].Name != "测试文件.iso" || items[0].Status != "active" {
		t.Fatalf("lost Unicode queue entry: %#v", items)
	}
}
