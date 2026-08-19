// reverse: billing 指纹算法逆向工具(实验)。
//
// 用真实抓包向量(data/dumps/*_downstream.json 自动提取)交叉约束搜索
// 新版 Claude Code CLI(2.1.195+/2.1.235)的 cc_version 指纹算法。
//
// 已测无解的空间(2026-08-19,4 向量):
//   - 3-char 索引[0,128) × 12 模式(含 salt 位置/session/device 变体)
//   - 2-char 索引[0,128)、4-char 索引[0,48) 主模式
//   - 切片模式 SHA256(salt?+T[i:i+N]+v), N∈{4..40}
//   - 取材:首条 user 首块 / 全块拼接 / 最后一条 user
//
// 已证明:fp 是 (首条user文本, 版本) 的确定性函数 —— 相同输入产生相同 fp,
// 不含随机数/时间成分。
// 结论:新版疑似更换了盐值或输入构成,简单结构空间不可逆;需分析新版
// CLI 的 js 源码或获取更多情报后再试。运行:go run ./cmd/reverse [dump目录]
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

const SALT = "59cf53e54c78"

type vector struct {
	Text, Version, FP, Session, Device string
}

func fpOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:3]
}

func first8(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func ch(t string, i int) byte {
	if i < len(t) {
		return t[i]
	}
	return '0'
}

func main() {
	// 读环境变量指定的 dump 目录,提取向量
	dir := "data/dumps"
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	entries, _ := os.ReadDir(dir)
	reVer := regexp.MustCompile(`cc_version=(\d+\.\d+\.\d+)\.([0-9a-f]{3})`)
	var vecs []vector
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), "_downstream.json") {
			continue
		}
		raw, err := os.ReadFile(dir + "/" + e.Name())
		if err != nil {
			continue
		}
		var dump struct{ Body string }
		if json.Unmarshal(raw, &dump) != nil {
			continue
		}
		var b struct {
			System   []struct{ Text string } `json:"system"`
			Metadata struct{ UserID string } `json:"metadata"`
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if json.Unmarshal([]byte(dump.Body), &b) != nil || len(b.System) == 0 {
			continue
		}
		m := reVer.FindStringSubmatch(b.System[0].Text)
		if m == nil {
			continue
		}
		v := vector{Version: m[1], FP: m[2]}
		// session/device
		var uid struct {
			DeviceID  string `json:"device_id"`
			SessionID string `json:"session_id"`
		}
		json.Unmarshal([]byte(b.Metadata.UserID), &uid)
		v.Device, v.Session = uid.DeviceID, uid.SessionID
		// 首条 user 首块 text
		for _, msg := range b.Messages {
			if msg.Role != "user" {
				continue
			}
			var s string
			if json.Unmarshal(msg.Content, &s) == nil {
				v.Text = s
				break
			}
			var blocks []struct{ Type, Text string }
			if json.Unmarshal(msg.Content, &blocks) == nil {
				for _, bl := range blocks {
					if bl.Type == "text" {
						v.Text = bl.Text
						break
					}
				}
			}
			break
		}
		if v.Text != "" {
			vecs = append(vecs, v)
			fmt.Printf("vec: v=%s fp=%s textlen=%d session=%s\n", v.Version, v.FP, len(v.Text), first8(v.Session))
		}
	}
	if len(vecs) < 2 {
		fmt.Println("not enough vectors")
		return
	}

	patterns := map[string]func(c, v, sess, dev string) string{
		"salt+c+v":      func(c, v, s, d string) string { return SALT + c + v },
		"salt+v+c":      func(c, v, s, d string) string { return SALT + v + c },
		"c+v":           func(c, v, s, d string) string { return c + v },
		"v+c":           func(c, v, s, d string) string { return v + c },
		"salt+c":        func(c, v, s, d string) string { return SALT + c },
		"salt+c+v+sess": func(c, v, s, d string) string { return SALT + c + v + s },
		"salt+c+sess":   func(c, v, s, d string) string { return SALT + c + s },
		"salt+c+sess+v": func(c, v, s, d string) string { return SALT + c + s + v },
		"salt+dev+c+v":  func(c, v, s, d string) string { return SALT + d + c + v },
		"salt+c+v+dev":  func(c, v, s, d string) string { return SALT + c + v + d },
		"c+sess":        func(c, v, s, d string) string { return c + s },
		"salt+sess+c":   func(c, v, s, d string) string { return SALT + s + c },
	}
	type hit struct {
		name    string
		i, j, k int
	}
	var hits []hit

	match := func(name string, fn func(c, v, s, d string) string, i, j, k int) bool {
		for _, vec := range vecs {
			c := string([]byte{ch(vec.Text, i), ch(vec.Text, j), ch(vec.Text, k)})
			if fpOf(fn(c, vec.Version, vec.Session, vec.Device)) != vec.FP {
				return false
			}
		}
		return true
	}

	// R1: 3-char 索引 [0,128) 全模式
	for name, fn := range patterns {
		for i := 0; i < 128; i++ {
			for j := 0; j < 128; j++ {
				for k := 0; k < 128; k++ {
					if match(name, fn, i, j, k) {
						hits = append(hits, hit{name, i, j, k})
						fmt.Printf("HIT R1 %s idx=(%d,%d,%d)\n", name, i, j, k)
					}
				}
			}
		}
	}
	// R2: 2-char 索引
	for name, fn := range patterns {
		for i := 0; i < 128; i++ {
			for j := 0; j < 128; j++ {
				ok := true
				for _, vec := range vecs {
					c := string([]byte{ch(vec.Text, i), ch(vec.Text, j)})
					if fpOf(fn(c, vec.Version, vec.Session, vec.Device)) != vec.FP {
						ok = false
						break
					}
				}
				if ok {
					fmt.Printf("HIT R2(2char) %s idx=(%d,%d)\n", name, i, j)
					hits = append(hits, hit{name, i, j, -1})
				}
			}
		}
	}
	// R3: 4-char 索引 [0,48) 两个主模式
	for _, name := range []string{"salt+c+v", "c+v"} {
		fn := patterns[name]
		for i := 0; i < 48; i++ {
			for j := 0; j < 48; j++ {
				for k := 0; k < 48; k++ {
					for l := 0; l < 48; l++ {
						ok := true
						for _, vec := range vecs {
							c := string([]byte{ch(vec.Text, i), ch(vec.Text, j), ch(vec.Text, k), ch(vec.Text, l)})
							if fpOf(fn(c, vec.Version, vec.Session, vec.Device)) != vec.FP {
								ok = false
								break
							}
						}
						if ok {
							fmt.Printf("HIT R3(4char) %s idx=(%d,%d,%d,%d)\n", name, i, j, k, l)
							hits = append(hits, hit{name, i, j, k})
						}
					}
				}
			}
		}
	}
	// R4: 切片模式 SHA256(SALT+T[i:i+N]+v) 与无盐版
	for _, withSalt := range []bool{true, false} {
		for n := 4; n <= 40; n += 4 {
			for i := 0; i < 96; i++ {
				ok := true
				for _, vec := range vecs {
					end := i + n
					if end > len(vec.Text) {
						end = len(vec.Text)
					}
					sub := vec.Text[i:end]
					in := sub + vec.Version
					if withSalt {
						in = SALT + sub + vec.Version
					}
					if fpOf(in) != vec.FP {
						ok = false
						break
					}
				}
				if ok {
					fmt.Printf("HIT R4(slice) salt=%v n=%d i=%d\n", withSalt, n, i)
				}
			}
		}
	}
	fmt.Println("done, hits:", len(hits))
}
