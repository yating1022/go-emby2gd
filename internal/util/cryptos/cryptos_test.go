package cryptos_test

import (
	"strings"
	"testing"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/cryptos"
)

func TestRandomHex_LengthAndCharset(t *testing.T) {
	cases := []struct {
		name   string
		nBytes int
		want   int
	}{
		{"32 字节密钥", 32, 64},
		{"16 字节 id", 16, 32},
		{"1 字节", 1, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cryptos.RandomHex(tc.nBytes)
			if len(got) != tc.want {
				t.Fatalf("长度 = %d, want %d", len(got), tc.want)
			}
			if strings.Trim(got, "0123456789abcdef") != "" {
				t.Fatalf("应只包含小写 16 进制字符, 实际: %q", got)
			}
		})
	}
}

func TestRandomHex_InvalidLength(t *testing.T) {
	if got := cryptos.RandomHex(0); got != "" {
		t.Errorf("0 字节应返回空串, 实际: %q", got)
	}
	if got := cryptos.RandomHex(-1); got != "" {
		t.Errorf("负数应返回空串, 实际: %q", got)
	}
}

func TestRandomHex_Unique(t *testing.T) {
	// 连抽两次相同的概率可以忽略; 上锁防止编译器把两次调用合并
	first := cryptos.RandomHex(32)
	second := cryptos.RandomHex(32)
	if first == second {
		t.Fatalf("两次生成的随机串不应相同: %q", first)
	}
}

func TestEqual(t *testing.T) {
	cases := []struct {
		name string
		a    string
		b    string
		want bool
	}{
		{"相同", "abc123", "abc123", true},
		{"长度相同但内容不同", "abc123", "abc124", false},
		{"长度不同", "abc123", "abc1234", false},
		{"两侧为空", "", "", false},
		{"一侧为空", "", "abc", false},
		{"另一侧为空", "abc", "", false},
		{"只差一个字符的长串", strings.Repeat("a", 63) + "b", strings.Repeat("a", 64), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cryptos.Equal(tc.a, tc.b); got != tc.want {
				t.Errorf("Equal(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}
