package viewpoints_test

import (
	"strings"
	"testing"

	"gateflow/builtins"
	"gateflow/internal/viewpoints"
)

func columns(t *testing.T) viewpoints.Columns {
	t.Helper()
	c, err := viewpoints.LoadColumns(builtins.FS())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestParse(t *testing.T) {
	csv := "\ufeffID,観点,確認の方法,雛形の値\nVP-01,最大文字数を超える入力を拒否する,関数,{最大文字数}\nVP-02,エラーの表示位置,画面,\n"
	tb, err := viewpoints.Parse([]byte(csv), columns(t))
	if err != nil {
		t.Fatal(err)
	}
	if tb.Rows != 2 || !tb.Has("VP-01") || tb.Has("VP-03") {
		t.Errorf("Rows %d", tb.Rows)
	}
}

func TestParse_Invalid(t *testing.T) {
	cases := map[string]struct{ csv, want string }{
		"必須の列がない":    {"ID,観点\nVP-01,x\n", "確認の方法"},
		"ID が重なる":    {"ID,観点,確認の方法\nVP-01,x,関数\nVP-01,y,画面\n", "重なっている"},
		"ID が空":      {"ID,観点,確認の方法\n,x,関数\n", "空"},
		"確認の方法が知らない": {"ID,観点,確認の方法\nVP-01,x,目視\n", "目視"},
		"CSV の誤り":    {"ID,観点,確認の方法\nVP-01,\"x,関数\n", "CSV"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := viewpoints.Parse([]byte(tc.csv), columns(t))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
}
