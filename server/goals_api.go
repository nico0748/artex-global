package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// goals_api.go 가 사용자에게 돌려주는 오류 응답 문구. 명령·요청 필드명(text·vulnclass·
// gid)·enum 은 원문 그대로 두고, 사람이 읽는 메시지만 한국어로 둔다. 영어 writeErr
// (task not found·invalid JSON·bad goal id)는 F3b 중국어 전용 범위라 보존한다.
const (
	errGoalTaskDeletingAdd    = "작업을 삭제하는 중이라 목표를 추가할 수 없습니다"
	errGoalTaskDeletingEdit   = "작업을 삭제하는 중이라 목표를 수정할 수 없습니다"
	errGoalTaskDeletingDelete = "작업을 삭제하는 중이라 목표를 삭제할 수 없습니다"
	errGoalTextEmpty          = "목표 내용은 비워 둘 수 없습니다"
	errGoalNotFound           = "목표를 찾을 수 없습니다"
	errGoalReadAfterAdd       = "목표를 저장한 뒤 읽지 못했습니다"
	errGoalReadAfterEdit      = "목표를 수정한 뒤 읽지 못했습니다"
)

// 概要の「目標の管理」の人による CRUD のインターフェース。agent 側の set_goals ツールと同じ goal ノードの群に書くが、
// 入口は人間が UI で直接追加・削除・変更すること;追加/変更の後は「タスクの復活」のロジックを再利用する(admitTask resume:
// 終端→running、一時停止を解く、必要なら待ち行列に入れる)。削除では復活させない(製品の決定による)。変更の handler はどれも
// beginTaskOperation/decInflight を通り、タスクの削除との競合を避ける(意図の CRUD と同じ)。

// listGoals はこのタスクのすべての目標(text/vulnclass/state に分けたもの)を返し、目標の管理のカードの描画に使う。
func (s *Server) listGoals(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	goals, err := t.Store.ListByKind(db.KindGoal, 10000)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"goals": goalDTOs(goals)})
}

// addGoal は人が目標を1つ追加する:保存(タスクの根の spawns の下にかける)→「目標を追加した」の起動を1件記録して
// planner を起こす → タスクを復活させ、計画者が新しい目標をもとに達成したかを判定し直す。
func (s *Server) addGoal(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, errGoalTaskDeletingAdd)
		return
	}
	defer s.engine.decInflight(t.ID)

	var body struct {
		Text      string `json:"text"`
		VulnClass string `json:"vulnclass"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeErr(w, 400, errGoalTextEmpty)
		return
	}
	payload := map[string]any{"text": text}
	if vc := strings.TrimSpace(body.VulnClass); vc != "" {
		payload["vulnclass"] = vc
	}
	id, err := t.Store.AddGoal(payload, "human")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if of, _ := t.Store.OriginFactID(); of > 0 && id > 0 {
		_ = t.Store.Link(of, db.RelSpawns, id) // goal descends from the task root (origin fact)
	}
	t.NotifyGoal([]string{text}) // 「人が目標を追加した:…」の起動を記録して planner を起こす
	s.reviveTask(t)              // 完了/一時停止したタスクを実行の状態に戻して続ける
	node, _ := t.Store.GetNode(id)
	if node == nil {
		writeErr(w, 500, errGoalReadAfterAdd)
		return
	}
	writeJSON(w, 200, goalDTO(node))
}

// editGoal は人が目標の文(と vulnclass)を1つ変える:DB を変える →「ユーザーが目標を old から new に変えた」を記録して
// planner を起こす → タスクを復活させ、計画者が新しい目標をもとに方向を調整する。
func (s *Server) editGoal(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, errGoalTaskDeletingEdit)
		return
	}
	defer s.engine.decInflight(t.ID)

	gid, err := strconv.ParseInt(r.PathValue("gid"), 10, 64)
	if err != nil || gid <= 0 {
		writeErr(w, 400, "bad goal id")
		return
	}
	var body struct {
		Text      string `json:"text"`
		VulnClass string `json:"vulnclass"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeErr(w, 400, errGoalTextEmpty)
		return
	}
	node, err := t.Store.GetNode(gid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if node == nil || node.Kind != db.KindGoal {
		writeErr(w, 404, errGoalNotFound)
		return
	}
	oldText := goalDTO(node).Text
	if err := t.Store.UpdateGoalPayload(gid, text, strings.TrimSpace(body.VulnClass)); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	t.NotifyGoalEdited(oldText, text) // 「人が目標を old から new に変えた」の起動を記録して planner を起こす
	s.reviveTask(t)                   // 追加と同じ:タスクを復活させ、新しい目標で判定し直す
	updated, _ := t.Store.GetNode(gid)
	if updated == nil {
		writeErr(w, 500, errGoalReadAfterEdit)
		return
	}
	writeJSON(w, 200, goalDTO(updated))
}

// deleteGoal は人が目標を1つ削除する(物理削除で、辺/錨も連鎖して消す):DB から消す →「ユーザーがその目標 X を削除した」を記録して
// planner を起こし、残りの目標をもとに判定し直させる。製品の決定により、削除ではタスクを【復活させない】。
func (s *Server) deleteGoal(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, errGoalTaskDeletingDelete)
		return
	}
	defer s.engine.decInflight(t.ID)

	gid, err := strconv.ParseInt(r.PathValue("gid"), 10, 64)
	if err != nil || gid <= 0 {
		writeErr(w, 400, "bad goal id")
		return
	}
	node, err := t.Store.GetNode(gid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if node == nil || node.Kind != db.KindGoal {
		writeErr(w, 404, errGoalNotFound)
		return
	}
	text := goalDTO(node).Text
	if err := t.Store.DeleteGoal(gid); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	t.NotifyGoalDeleted(text) // 「人がその目標を削除した:…」の起動を記録して planner を起こす(タスクは復活させない)
	writeJSON(w, 200, map[string]bool{"ok": true})
}
