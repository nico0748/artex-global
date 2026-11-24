package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// プラットフォームの操作のツール(組み込みの Auto agent 用):skill、カスタムのツール、MCP を作る/変える。どれも host ツールで、
// tools 表に種まきし、既定で auto に結びつけ、hostTools で注入する。既存の db/ファイルシステムのロジックを再利用する。

func (s *Server) platformTools() []actool.CoreTool {
	return []actool.CoreTool{
		s.toolCreateSkill(),
		s.toolUpdateSkillFile(),
		s.toolCreateCustomTool(),
		s.toolUpdateCustomTool(),
		s.toolCreateMCP(),
		s.toolUpdateMCP(),
		s.toolDeleteAssetsByHost(),
	}
}

// platformToolKeys are the tool keys the Auto agent gets bound by default.
var platformToolKeys = []string{
	"create_skill", "update_skill_file",
	"create_custom_tool", "update_custom_tool",
	"create_mcp", "update_mcp",
	"delete_assets_by_host",
}

// ---- assets ----

// toolDeleteAssetsByHost hard-deletes every asset tied to one host (exact match).
// Platform-level (not a per-task tool): operates on the global, cross-task 資産の DB.
func (s *Server) toolDeleteAssetsByHost() actool.CoreTool {
	return wrTool("delete_assets_by_host",
		"host を正確に指定して資産を削除する：その host のドメイン/サブドメインと、その下のサービス(service)、インターフェース(endpoint)を消す。\n"+
			"host は完全一致(小文字にし、空白を除く)で、あいまい/ワイルドカードではない。\n"+
			"ルートドメイン(example.com など)を渡すと、そのサブドメインとそのサービス/インターフェースも一緒に削除する；サブドメイン(a.example.com など)や IP を渡すと、その host 自身とそのサービス/インターフェースだけを消す。\n"+
			"⚠️ 物理削除で、全体の資産の DB(タスクをまたいで共有)に作用し、取り消せない。",
		objSchema(map[string]any{
			"host": strParam("削除する host：ドメイン/サブドメイン/IP。完全一致。例 example.com、a.example.com、1.2.3.4"),
		}, "host"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			as := s.assetStore()
			if as == nil {
				return actool.Errorf("資産の DB が初期化されていない"), nil
			}
			var a struct {
				Host string `json:"host"`
			}
			_ = json.Unmarshal(in, &a)
			if strings.TrimSpace(a.Host) == "" {
				return actool.Errorf("host は空にできない"), nil
			}
			counts, err := as.DeleteByHost(a.Host)
			if err != nil {
				return actool.Errorf("削除に失敗: " + err.Error()), nil
			}
			var total int64
			for _, n := range counts {
				total += n
			}
			return jsonResult(map[string]any{
				"host":            a.Host,
				"deleted":         total,
				"deleted_by_type": counts,
			})
		})
}

// ---- skills ----

func (s *Server) toolCreateSkill() actool.CoreTool {
	return wrTool("create_skill",
		"新しい skill を1つ作る(SKILL.md を書く。agentskills.io の規格)。name は小文字の英字/数字/ハイフン。",
		objSchema(map[string]any{
			"name":         strParam("skill の名前(小文字の英字で始まり、英字/数字/ハイフン)"),
			"description":  strParam("skill の説明(必須。何をするか/いつ使うかを説明する)"),
			"instructions": strParam("Markdown の本文の説明(任意)"),
		}, "name", "description"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct{ Name, Description, Instructions string }
			_ = json.Unmarshal(in, &a)
			if !validSkillName(a.Name) {
				return actool.Errorf("skill の名前が不正(小文字の英字で始まり、英字/数字/ハイフンだけ、≤64)"), nil
			}
			if strings.TrimSpace(a.Description) == "" {
				return actool.Errorf("description は必須"), nil
			}
			path := filepath.Join(s.skillDir, a.Name)
			if _, err := os.Stat(path); err == nil {
				return actool.Errorf("skill はすでに存在する: " + a.Name), nil
			}
			if err := os.MkdirAll(path, 0o755); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			var b strings.Builder
			b.WriteString("---\n")
			fmt.Fprintf(&b, "name: %s\n", a.Name)
			fmt.Fprintf(&b, "description: %s\n", a.Description)
			b.WriteString("---\n")
			if strings.TrimSpace(a.Instructions) != "" {
				b.WriteString(a.Instructions)
			} else {
				fmt.Fprintf(&b, "## %s\n\n1. \n2. \n3. \n", a.Name)
			}
			if err := os.WriteFile(filepath.Join(path, "SKILL.md"), []byte(b.String()), 0o644); err != nil {
				_ = os.RemoveAll(path)
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("skill created: " + a.Name), nil
		})
}

func (s *Server) toolUpdateSkillFile() actool.CoreTool {
	return wrTool("update_skill_file",
		"ある skill の中のファイルを1つ書く/上書きする(既定は SKILL.md)。スキルの内容を変えたり、スクリプト/参照を足したりするのに使う。",
		objSchema(map[string]any{
			"name":    strParam("skill の名前"),
			"file":    strParam("相対パス(任意。既定は SKILL.md。例 scripts/run.py)"),
			"content": strParam("ファイルの完全な内容"),
		}, "name", "content"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct{ Name, File, Content string }
			_ = json.Unmarshal(in, &a)
			if !validSkillName(a.Name) {
				return actool.Errorf("skill の名前が不正"), nil
			}
			skillPath := filepath.Join(s.skillDir, a.Name)
			if _, err := os.Stat(skillPath); os.IsNotExist(err) {
				return actool.Errorf("skill が存在しない: " + a.Name), nil
			}
			rel := strings.TrimSpace(a.File)
			if rel == "" {
				rel = "SKILL.md"
			}
			clean, msg := skillRelPath(rel)
			if msg != "" {
				return actool.Errorf("不正なパス: " + msg), nil
			}
			full := filepath.Join(skillPath, clean)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if err := os.WriteFile(full, []byte(a.Content), 0o644); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("skill file written: " + a.Name + "/" + clean), nil
		})
}

// ---- custom tools ----

type customToolToolInput struct {
	Key         string          `json:"key"`
	Description string          `json:"description"`
	Kind        string          `json:"kind"`
	Exec        json.RawMessage `json:"exec"`
	Schema      json.RawMessage `json:"schema"`
	Agents      []string        `json:"agents"`
	Deferred    bool            `json:"deferred"`
	Enabled     *bool           `json:"enabled"`
}

func customToolSchema(keyDesc string) map[string]any {
	return objSchema(map[string]any{
		"key":         strParam(keyDesc),
		"description": strParam("モデルに渡す説明"),
		"kind":        strParam("shell | command | script(Python だけ) | http。shell=bash の環境の宣言(そのツールを bash で直接呼べることをモデルに知らせるだけで、exec/schema は不要)；他の3種類は exec が必要"),
		"exec":        map[string]any{"type": "object", "description": "実行の仕様(shell の種類では不要): command→{command}; script→{code}; http→{method,url,headers,body,proxy,use_recording_proxy}"},
		"schema":      map[string]any{"type": "object", "description": "引数の JSON-Schema(shell/command/script は空でよい; http は必須で properties を含むこと)"},
		"agents":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "結びつける agent の key(任意)"},
		"deferred":    map[string]any{"type": "boolean", "description": "遅延させるか(shell の種類では無効；command/script/http のあまり使わないツールだけオンにする)"},
		"enabled":     map[string]any{"type": "boolean", "description": "有効にするか(既定は true)"},
	}, "key", "kind")
}

func toDBTool(a customToolToolInput) *db.Tool {
	enabled := true
	if a.Enabled != nil {
		enabled = *a.Enabled
	}
	return &db.Tool{
		Key: a.Key, Description: a.Description, Schema: a.Schema, Agents: a.Agents,
		Enabled: enabled, Kind: a.Kind, Exec: a.Exec, Deferred: a.Deferred,
	}
}

func (s *Server) toolCreateCustomTool() actool.CoreTool {
	return wrTool("create_custom_tool", "【重要】プラットフォームにないツールをインストールしたら、このツールを呼んでインストールしたツールをプラットフォームに入れ、プラットフォームから呼べるようにすること！カスタムのツール(shell/command/script/http)を1つ作る。shell=bash の環境の宣言で、key+description+agents だけが必要で、exec/schema は不要。",
		customToolSchema("ツールの key(小文字の英字で始まり、英字/数字/アンダースコア)"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a customToolToolInput
			_ = json.Unmarshal(in, &a)
			a.Key = strings.TrimSpace(a.Key)
			if !reToolKey.MatchString(a.Key) {
				return actool.Errorf("key は小文字の英字で始め、小文字の英字/数字/アンダースコアだけを含むこと"), nil
			}
			if a.Kind != "shell" && a.Kind != "command" && a.Kind != "script" && a.Kind != "http" {
				return actool.Errorf("kind は shell / command / script / http のどれかでなければならない"), nil
			}
			if a.Kind == "http" && !hasSchemaProps(a.Schema) {
				return actool.Errorf("http のツールは引数の JSON Schema を必ず渡すこと(空にできない)"), nil
			}
			if exist, _ := s.m.pg.GetTool(a.Key); exist != nil {
				return actool.Errorf("その key はすでに存在する: " + a.Key), nil
			}
			if err := s.m.pg.CreateCustomTool(toDBTool(a)); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("custom tool created: " + a.Key), nil
		})
}

func (s *Server) toolUpdateCustomTool() actool.CoreTool {
	return wrTool("update_custom_tool", "既存のカスタムのツールを1つ変える(key で指定)。",
		customToolSchema("変えるカスタムのツールの key"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a customToolToolInput
			_ = json.Unmarshal(in, &a)
			existing, _ := s.m.pg.GetTool(a.Key)
			if existing == nil || existing.System {
				return actool.Errorf("変えられるのはカスタムのツールだけ: " + a.Key), nil
			}
			if a.Kind != "shell" && a.Kind != "command" && a.Kind != "script" && a.Kind != "http" {
				return actool.Errorf("kind は shell / command / script / http のどれかでなければならない"), nil
			}
			if a.Kind == "http" && !hasSchemaProps(a.Schema) {
				return actool.Errorf("http のツールは引数の JSON Schema を必ず渡すこと(空にできない)"), nil
			}
			if err := s.m.pg.UpdateCustomTool(toDBTool(a)); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("custom tool updated: " + a.Key), nil
		})
}

// ---- MCP ----

type mcpToolInput struct {
	ID        int64           `json:"id"`
	Name      string          `json:"name"`
	Transport string          `json:"transport"`
	Command   string          `json:"command"`
	Args      json.RawMessage `json:"args"`
	Env       json.RawMessage `json:"env"`
	URL       string          `json:"url"`
	Enabled   *bool           `json:"enabled"`
	Insecure  *bool           `json:"insecure"`
}

func mcpSchema(withID bool) map[string]any {
	props := map[string]any{
		"name":      strParam("MCP サーバーの名前"),
		"transport": strParam("stdio | http / sse"),
		"command":   strParam("stdio の起動のコマンド(npx など)"),
		"args":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "コマンドの引数の配列"},
		"env":       map[string]any{"type": "object", "description": "環境変数 {KEY:VALUE}"},
		"url":       strParam("http/sse の URL"),
		"enabled":   map[string]any{"type": "boolean", "description": "有効にするか(既定は true)"},
		"insecure":  map[string]any{"type": "boolean", "description": "http: TLS の証明書の検証を飛ばす(自己署名の証明書なら true にする。既定は false)"},
	}
	required := []string{"name", "transport"}
	if withID {
		props["id"] = map[string]any{"type": "integer", "description": "変える MCP サーバーの id"}
		required = []string{"id", "name", "transport"}
	}
	return objSchema(props, required...)
}

func (a mcpToolInput) toDB() *db.MCPServer {
	enabled := true
	if a.Enabled != nil {
		enabled = *a.Enabled
	}
	insecure := false
	if a.Insecure != nil {
		insecure = *a.Insecure
	}
	return &db.MCPServer{
		ID: a.ID, Name: a.Name, Transport: a.Transport, Command: a.Command,
		Args: a.Args, Env: a.Env, URL: a.URL, Enabled: enabled, Insecure: insecure,
	}
}

func (s *Server) toolCreateMCP() actool.CoreTool {
	return wrTool("create_mcp", "MCP サーバー(stdio/http/sse)を1つ作る。作った後、そのツールは agent ごとの見え方で許可する必要がある。",
		mcpSchema(false),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a mcpToolInput
			_ = json.Unmarshal(in, &a)
			a.ID = 0
			if strings.TrimSpace(a.Name) == "" || strings.TrimSpace(a.Transport) == "" {
				return actool.Errorf("name / transport は必須"), nil
			}
			id, err := s.m.pg.SaveMCP(a.toDB())
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text(fmt.Sprintf("mcp created: id=%d name=%s", id, a.Name)), nil
		})
}

func (s *Server) toolUpdateMCP() actool.CoreTool {
	return wrTool("update_mcp", "既存の MCP サーバーを1つ変える(id で指定)。",
		mcpSchema(true),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a mcpToolInput
			_ = json.Unmarshal(in, &a)
			if a.ID == 0 {
				return actool.Errorf("id は必須"), nil
			}
			if _, err := s.m.pg.SaveMCP(a.toDB()); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text(fmt.Sprintf("mcp updated: id=%d", a.ID)), nil
		})
}
