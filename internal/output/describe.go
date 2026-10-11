package output

import (
	"fmt"
	"reflect"
	"strings"
)

// describe は、構造体のタグから出力形式の説明（Markdown）を作る。
// json のタグを項目の名前、desc を説明、enum を選択肢として使う。
func describe(path string, v any) string {
	var b strings.Builder
	fmt.Fprintf(&b, "結果は `%s` に、次の項目を持つ1つの JSON のオブジェクトとして書く。ここにない項目は書かない。\n\n", path)
	describeStruct(&b, reflect.TypeOf(v), "")
	return b.String()
}

func describeStruct(b *strings.Builder, t reflect.Type, prefix string) {
	for i := range t.NumField() {
		f := t.Field(i)
		name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		path := prefix + name
		req := "必須"
		if strings.Contains(opts, "omitempty") {
			req = "任意"
		}
		fmt.Fprintf(b, "- `%s`（%s、%s）：%s", path, typeName(f.Type), req, f.Tag.Get("desc"))
		if enum := f.Tag.Get("enum"); enum != "" {
			vals := strings.Split(enum, ",")
			for j := range vals {
				vals[j] = "`" + vals[j] + "`"
			}
			fmt.Fprintf(b, "。選択肢：%s", strings.Join(vals, "・"))
		}
		b.WriteString("\n")
		switch et := f.Type; {
		case et.Kind() == reflect.Slice && et.Elem().Kind() == reflect.Struct:
			describeStruct(b, et.Elem(), path+"[].")
		case et.Kind() == reflect.Pointer && et.Elem().Kind() == reflect.Struct:
			describeStruct(b, et.Elem(), path+".")
		}
	}
}

func typeName(t reflect.Type) string {
	switch t.Kind() {
	case reflect.String:
		return "文字列"
	case reflect.Pointer:
		if t.Elem().Kind() == reflect.Struct {
			return "オブジェクト"
		}
		return typeName(t.Elem())
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Struct {
			return "オブジェクトの配列"
		}
		return typeName(t.Elem()) + "の配列"
	default:
		return t.Kind().String()
	}
}
