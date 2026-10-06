package web

import (
	"errors"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"

	"pagehut/internal/sitefs"
	"pagehut/internal/storage"
	"pagehut/internal/store"
)

// sanitizeName 清理用户提供的文件/目录名。
func sanitizeName(name string) string {
	name = strings.TrimSpace(name)
	// 取纯文件名部分，防路径混入
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	if name == "." || name == ".." || name == "/" || name == "" {
		return ""
	}
	if len(name) > 255 {
		return ""
	}
	for _, c := range name {
		if c < 0x20 || c == 0x7f {
			return ""
		}
	}
	return name
}

func (w *Web) canViewProject(rw http.ResponseWriter, r *http.Request) (*store.User, *store.Project, bool) {
	u, ok := w.auth(rw, r)
	if !ok {
		return nil, nil, false
	}
	p, ok := w.loadProject(rw, r, u)
	if !ok {
		return nil, nil, false
	}
	return u, p, true
}

// filesPage 文件管理器（?dir= 相对目录）。
func (w *Web) filesPage(rw http.ResponseWriter, r *http.Request) {
	u, p, ok := w.canViewProject(rw, r)
	if !ok {
		return
	}
	dir := strings.TrimPrefix(r.URL.Query().Get("dir"), "/")
	if dir == "" {
		dir = "."
	}
	entries, err := w.disk.ListDir(p.ID, dir)
	if err != nil {
		if errors.Is(err, storage.ErrBadPath) {
			w.errorPage(rw, r, http.StatusBadRequest, "非法路径")
			return
		}
		w.errorPage(rw, r, http.StatusNotFound, "目录不存在")
		return
	}
	// 面包屑
	crumbs := []map[string]string{{"Name": p.Slug, "Path": "."}}
	if dir != "." {
		acc := ""
		for _, part := range strings.Split(strings.Trim(dir, "/"), "/") {
			if part == "" {
				continue
			}
			acc = path.Join(acc, part)
			crumbs = append(crumbs, map[string]string{"Name": part, "Path": acc})
		}
	}
	w.render(rw, r, http.StatusOK, "files", "文件管理", map[string]any{
		"Project":   p,
		"Dir":       dir,
		"Crumbs":    crumbs,
		"Entries":   entries,
		"CanManage": canManageProject(u, p),
	})
}

// fileEditPage 在线编辑文本文件（?path=）。
func (w *Web) fileEditPage(rw http.ResponseWriter, r *http.Request) {
	u, p, ok := w.canViewProject(rw, r)
	if !ok {
		return
	}
	fp := strings.TrimPrefix(r.URL.Query().Get("path"), "/")
	if fp == "" {
		w.errorPage(rw, r, http.StatusBadRequest, "缺少文件路径")
		return
	}
	if !sitefs.IsTextFile(fp) {
		flash(rw, r, "该文件类型不支持在线编辑，可下载后本地编辑。")
		http.Redirect(rw, r, "/projects/"+strconv.FormatInt(p.ID, 10)+"/files?dir="+parentDir(fp), http.StatusSeeOther)
		return
	}
	content, err := w.disk.ReadFile(p.ID, fp)
	if err != nil {
		w.errorPage(rw, r, http.StatusNotFound, "文件不存在或过大（在线编辑仅支持 4MB 内文本文件）")
		return
	}
	w.render(rw, r, http.StatusOK, "fileedit", "编辑文件", map[string]any{
		"Project":   p,
		"Path":      fp,
		"Content":   string(content),
		"CanManage": canManageProject(u, p),
	})
}

// fileSave 保存编辑内容。
func (w *Web) fileSave(rw http.ResponseWriter, r *http.Request) {
	u, p, ok := w.canViewProject(rw, r)
	if !ok {
		return
	}
	if !canManageProject(u, p) {
		w.errorPage(rw, r, http.StatusForbidden, "没有权限修改该项目")
		return
	}
	fp := strings.TrimPrefix(r.FormValue("path"), "/")
	content := r.FormValue("content")
	if fp == "" {
		w.errorPage(rw, r, http.StatusBadRequest, "缺少文件路径")
		return
	}
	st, _ := w.st.GetSettings()
	_, sizeLimit := w.quotaFor(u, st)

	oldSize := int64(0)
	if fi, err := w.disk.Stat(p.ID, fp); err == nil && !fi.IsDir() {
		oldSize = fi.Size()
	}
	if p.Size-oldSize+int64(len(content)) > sizeLimit {
		w.errorPage(rw, r, http.StatusBadRequest, "保存后超出单项目大小上限（"+byteSize(sizeLimit)+"）。")
		return
	}
	if err := w.disk.WriteFile(p.ID, fp, []byte(content)); err != nil {
		w.errorPage(rw, r, http.StatusBadRequest, "保存失败: "+err.Error())
		return
	}
	w.contentChanged(p, st)
	flash(rw, r, "文件已保存。")
	http.Redirect(rw, r, "/projects/"+strconv.FormatInt(p.ID, 10)+"/files?dir="+parentDir(fp), http.StatusSeeOther)
}

// fileUpload 在文件管理器中上传单个文件。
func (w *Web) fileUpload(rw http.ResponseWriter, r *http.Request) {
	u, p, ok := w.canViewProject(rw, r)
	if !ok {
		return
	}
	if !canManageProject(u, p) {
		w.errorPage(rw, r, http.StatusForbidden, "没有权限修改该项目")
		return
	}
	dir := strings.TrimPrefix(r.FormValue("dir"), "/")
	if dir == "" {
		dir = "."
	}
	st, _ := w.st.GetSettings()
	_, sizeLimit := w.quotaFor(u, st)
	remaining := sizeLimit - p.Size
	if remaining <= 0 {
		w.errorPage(rw, r, http.StatusBadRequest, "项目空间已用完（上限 "+byteSize(sizeLimit)+"）。")
		return
	}
	r.Body = http.MaxBytesReader(rw, r.Body, remaining+(64<<10))
	file, header, err := r.FormFile("file")
	if err != nil {
		w.errorPage(rw, r, http.StatusBadRequest, "请选择要上传的文件（或文件超出剩余空间）。")
		return
	}
	defer file.Close()
	name := sanitizeName(header.Filename)
	if name == "" {
		w.errorPage(rw, r, http.StatusBadRequest, "文件名不合法。")
		return
	}
	data, err := io.ReadAll(io.LimitReader(file, remaining+1))
	if err != nil {
		w.errorPage(rw, r, http.StatusBadRequest, "读取上传内容失败。")
		return
	}
	if int64(len(data)) > remaining {
		w.errorPage(rw, r, http.StatusBadRequest, "文件超出剩余项目空间（剩余 "+byteSize(remaining)+"）。")
		return
	}
	rel := name
	if dir != "." {
		rel = path.Join(dir, name)
	}
	if err := w.disk.WriteFile(p.ID, rel, data); err != nil {
		w.errorPage(rw, r, http.StatusBadRequest, "写入失败: "+err.Error())
		return
	}
	w.contentChanged(p, st)
	flash(rw, r, "文件已上传。")
	http.Redirect(rw, r, "/projects/"+strconv.FormatInt(p.ID, 10)+"/files?dir="+dir, http.StatusSeeOther)
}

// fileMkdir 新建目录。
func (w *Web) fileMkdir(rw http.ResponseWriter, r *http.Request) {
	u, p, ok := w.canViewProject(rw, r)
	if !ok {
		return
	}
	if !canManageProject(u, p) {
		w.errorPage(rw, r, http.StatusForbidden, "没有权限修改该项目")
		return
	}
	dir := strings.TrimPrefix(r.FormValue("dir"), "/")
	name := sanitizeName(r.FormValue("name"))
	if name == "" {
		w.errorPage(rw, r, http.StatusBadRequest, "目录名不合法。")
		return
	}
	rel := name
	if dir != "." {
		rel = path.Join(dir, name)
	}
	if err := w.disk.Mkdir(p.ID, rel); err != nil {
		w.errorPage(rw, r, http.StatusBadRequest, "创建目录失败: "+err.Error())
		return
	}
	flash(rw, r, "目录已创建。")
	http.Redirect(rw, r, "/projects/"+strconv.FormatInt(p.ID, 10)+"/files?dir="+rel, http.StatusSeeOther)
}

// fileDelete 删除文件或目录。
func (w *Web) fileDelete(rw http.ResponseWriter, r *http.Request) {
	u, p, ok := w.canViewProject(rw, r)
	if !ok {
		return
	}
	if !canManageProject(u, p) {
		w.errorPage(rw, r, http.StatusForbidden, "没有权限修改该项目")
		return
	}
	dir := strings.TrimPrefix(r.FormValue("dir"), "/")
	if dir == "" {
		dir = "."
	}
	fp := strings.TrimPrefix(r.FormValue("path"), "/")
	if fp == "" {
		w.errorPage(rw, r, http.StatusBadRequest, "缺少文件路径")
		return
	}
	if err := w.disk.Remove(p.ID, fp); err != nil {
		w.errorPage(rw, r, http.StatusBadRequest, "删除失败: "+err.Error())
		return
	}
	w.contentChanged(p, w.mustSettings())
	flash(rw, r, "已删除。")
	http.Redirect(rw, r, "/projects/"+strconv.FormatInt(p.ID, 10)+"/files?dir="+dir, http.StatusSeeOther)
}

// fileDownload 下载项目内文件。
func (w *Web) fileDownload(rw http.ResponseWriter, r *http.Request) {
	_, p, ok := w.canViewProject(rw, r)
	if !ok {
		return
	}
	fp := strings.TrimPrefix(r.URL.Query().Get("path"), "/")
	if fp == "" {
		w.errorPage(rw, r, http.StatusBadRequest, "缺少文件路径")
		return
	}
	f, fi, err := w.disk.OpenFile(p.ID, fp)
	if err != nil {
		w.errorPage(rw, r, http.StatusNotFound, "文件不存在")
		return
	}
	defer f.Close()
	rw.Header().Set("Content-Type", sitefs.ContentType(fp))
	http.ServeContent(rw, r, path.Base(fp), fi.ModTime(), f)
}
