package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// 概要の「制約の管理」の人による CRUD のインターフェース + 注入の範囲の切り替えの解決。操作の制約(allow/deny)は agent 側の
// set_constraints ツールと同じ task_constraints 表に書く;ここは人間が UI で直接追加・削除・変更する。制約は
// プロンプトのコンテキストにすぎない——追加・削除・変更の後に planner には【知らせず】、次のラウンドの計画で自然に DB を読んで効く(製品の決定による)。変更の handler は
// どれも beginTaskOperation/decInflight を通り、タスクの削除との競合を避ける(目標/意図の CRUD と同じ)。

// 注入の範囲の切り替えの settings の key。既定はどれもオン(GetBool の第2引数 = true)。
const (
	settingConstraintsInjectPlanner = "constraints_inject_planner"
	settingConstraintsInjectWorker  = "constraints_inject_worker"
)

// 制約 CRUD 핸들러의 사용자 노출 에러 응답(한국어). writeErr 로 그대로 UI 토스트에 노출된다.
// 用語: 制約→제약(ko.json 의 약속/제약 표기와 정합), タスク→작업. 추가/수정/삭제 문구는
// goals_api.go 의 errGoalTaskDeleting* 와 같은 문형이고, kind 검증은 intercept.go 의
// "값은 … 중 하나여야 합니다" 패턴을 따른다.
const (
	errConstraintTaskDeletingAdd    = "작업을 삭제하는 중이라 제약을 추가할 수 없습니다"
	errConstraintTaskDeletingEdit   = "작업을 삭제하는 중이라 제약을 수정할 수 없습니다"
	errConstraintTaskDeletingDelete = "작업을 삭제하는 중이라 제약을 삭제할 수 없습니다"
	errConstraintTextEmpty          = "제약 내용은 비워 둘 수 없습니다"
	errConstraintKindInvalid        = "kind 값은 allow 또는 deny 중 하나여야 합니다"
)

// constraintInjectPlanner / constraintInjectWorker は、操作の制約を対応する agent の
// システムプロンプトに注入するかを報告する(既定はオン)。resolver として planner/worker に渡し、ラウンドごとに読む → 切り替えを変えればすぐ効く。
func (s *Server) constraintInjectPlanner() bool {
	return s.m.pg.GetBool(settingConstraintsInjectPlanner, true)
}

func (s *Server) constraintInjectWorker() bool {
	return s.m.pg.GetBool(settingConstraintsInjectWorker, true)
}

// listConstraints はこのタスクのすべての操作の制約(allow が先、deny が後)を返す。
func (s *Server) listConstraints(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	rows, err := t.Store.ListConstraints()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"constraints": constraintDTOs(rows)})
}

// addConstraint は人が操作の制約を1件追加する(kind=allow|deny)。planner には知らせない。
func (s *Server) addConstraint(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, errConstraintTaskDeletingAdd)
		return
	}
	defer s.engine.decInflight(t.ID)

	var body struct {
		Text string `json:"text"`
		Kind string `json:"kind"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeErr(w, 400, errConstraintTextEmpty)
		return
	}
	kind := normalizeConstraintKind(body.Kind)
	if kind == "" {
		writeErr(w, 400, errConstraintKindInvalid)
		return
	}
	id, err := t.Store.AddConstraint(kind, text, "human")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, ConstraintDTO{ID: strconv.FormatInt(id, 10), Kind: kind, Text: text, Origin: "human"})
}

// editConstraint は人が制約を1件変える(kind + text)。planner には知らせない。
func (s *Server) editConstraint(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, errConstraintTaskDeletingEdit)
		return
	}
	defer s.engine.decInflight(t.ID)

	cid, err := strconv.ParseInt(r.PathValue("cid"), 10, 64)
	if err != nil || cid <= 0 {
		writeErr(w, 400, "bad constraint id")
		return
	}
	var body struct {
		Text string `json:"text"`
		Kind string `json:"kind"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeErr(w, 400, errConstraintTextEmpty)
		return
	}
	kind := normalizeConstraintKind(body.Kind)
	if kind == "" {
		writeErr(w, 400, errConstraintKindInvalid)
		return
	}
	if err := t.Store.UpdateConstraint(cid, kind, text); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, ConstraintDTO{ID: strconv.FormatInt(cid, 10), Kind: kind, Text: text})
}

// deleteConstraint は人が制約を1件削除する。planner には知らせない。
func (s *Server) deleteConstraint(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, errConstraintTaskDeletingDelete)
		return
	}
	defer s.engine.decInflight(t.ID)

	cid, err := strconv.ParseInt(r.PathValue("cid"), 10, 64)
	if err != nil || cid <= 0 {
		writeErr(w, 400, "bad constraint id")
		return
	}
	if err := t.Store.DeleteConstraint(cid); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// normalizeConstraintKind lowercases + validates the kind; "" on invalid.
func normalizeConstraintKind(k string) string {
	k = strings.TrimSpace(strings.ToLower(k))
	if k == "allow" || k == "deny" {
		return k
	}
	return ""
}

// constraintDTOs converts db rows to the frontend shape.
func constraintDTOs(in []db.Constraint) []ConstraintDTO {
	out := make([]ConstraintDTO, 0, len(in))
	for _, c := range in {
		out = append(out, ConstraintDTO{
			ID:     strconv.FormatInt(c.ID, 10),
			Kind:   c.Kind,
			Text:   c.Text,
			Origin: c.Origin,
			TS:     rfc3339(c.CreatedAt),
		})
	}
	return out
}
