package web

import (
	"net/http"
	"strconv"
	"strings"

	"pagehut/internal/store"
)

// ---------- 审核 ----------

func (w *Web) reviewQueue(rw http.ResponseWriter, r *http.Request) {
	u, ok := w.auth(rw, r, "reviewer", "admin")
	if !ok {
		return
	}
	list, err := w.st.ListProjectsByStatus(store.StatusPending)
	if err != nil {
		w.errorPage(rw, r, http.StatusInternalServerError, "服务器错误")
		return
	}
	w.render(rw, r, http.StatusOK, "review_queue", "审核队列", map[string]any{
		"List":    list,
		"IsAdmin": u.IsAdmin(),
	})
}

func (w *Web) reviewDetail(rw http.ResponseWriter, r *http.Request) {
	u, ok := w.auth(rw, r, "reviewer", "admin")
	if !ok {
		return
	}
	id := pathID(rw, r, "id")
	if id == 0 {
		return
	}
	p, err := w.st.GetProject(id)
	if err != nil || p == nil {
		w.errorPage(rw, r, http.StatusNotFound, "项目不存在")
		return
	}
	history, _ := w.st.ListReviewsByProject(p.ID)
	files, _, err := w.disk.Usage(p.ID)
	if err != nil {
		files = 0
	}
	w.render(rw, r, http.StatusOK, "review_detail", "审核项目", map[string]any{
		"Project": p,
		"History": history,
		"Files":   files,
		"IsAdmin": u.IsAdmin(),
	})
}

// reviewAction 审核动作：approve / reject（审核员、管理员）；
// publish / suspend（仅管理员，可对任意状态强制上/下架）。
func (w *Web) reviewAction(rw http.ResponseWriter, r *http.Request) {
	u, ok := w.auth(rw, r, "reviewer", "admin")
	if !ok {
		return
	}
	id := pathID(rw, r, "id")
	if id == 0 {
		return
	}
	p, err := w.st.GetProject(id)
	if err != nil || p == nil {
		w.errorPage(rw, r, http.StatusNotFound, "项目不存在")
		return
	}
	action := r.FormValue("action")
	reason := strings.TrimSpace(r.FormValue("reason"))

	fail := func(msg string) {
		flash(rw, r, msg)
		http.Redirect(rw, r, "/review/"+strconv.FormatInt(p.ID, 10), http.StatusSeeOther)
	}

	switch action {
	case "approve":
		if p.Status != store.StatusPending {
			fail("该项目不在待审核状态。")
			return
		}
		if err := w.st.UpdateProjectStatus(p.ID, store.StatusPublished, "", &u.ID); err != nil {
			fail("操作失败。")
			return
		}
		w.st.AddReview(p.ID, u.ID, "approve", reason)
		w.st.Notify(p.OwnerID, "项目审核通过", "项目「"+p.Name+"」("+p.Slug+") 已通过审核并发布。")
		w.st.Audit(&u.ID, u.Username, "project.review", "approve "+p.Slug)
		flash(rw, r, "已通过并发布。")

	case "reject":
		if p.Status != store.StatusPending {
			fail("该项目不在待审核状态。")
			return
		}
		if reason == "" {
			fail("驳回时必须填写理由。")
			return
		}
		if err := w.st.UpdateProjectStatus(p.ID, store.StatusRejected, reason, &u.ID); err != nil {
			fail("操作失败。")
			return
		}
		w.st.AddReview(p.ID, u.ID, "reject", reason)
		w.st.Notify(p.OwnerID, "项目审核未通过", "项目「"+p.Name+"」("+p.Slug+") 未通过审核，理由："+reason)
		w.st.Audit(&u.ID, u.Username, "project.review", "reject "+p.Slug+" | "+reason)
		flash(rw, r, "已驳回。")

	case "publish":
		if !u.IsAdmin() {
			w.errorPage(rw, r, http.StatusForbidden, "只有管理员可以强制发布")
			return
		}
		w.st.UpdateProjectStatus(p.ID, store.StatusPublished, "", &u.ID)
		w.st.Notify(p.OwnerID, "项目已发布", "项目「"+p.Name+"」("+p.Slug+") 已由管理员发布。")
		w.st.Audit(&u.ID, u.Username, "project.publish", p.Slug)
		flash(rw, r, "已强制发布。")

	case "suspend":
		if !u.IsAdmin() {
			w.errorPage(rw, r, http.StatusForbidden, "只有管理员可以下架项目")
			return
		}
		w.st.UpdateProjectStatus(p.ID, store.StatusSuspended, reason, &u.ID)
		note := "项目「" + p.Name + "」(" + p.Slug + ") 已被下架。"
		if reason != "" {
			note += "原因：" + reason
		}
		w.st.Notify(p.OwnerID, "项目已下架", note)
		w.st.Audit(&u.ID, u.Username, "project.suspend", p.Slug+" | "+reason)
		flash(rw, r, "已下架。")

	default:
		fail("未知操作。")
		return
	}
	http.Redirect(rw, r, "/review/"+strconv.FormatInt(p.ID, 10), http.StatusSeeOther)
}
