package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

type Settings struct {
	OpenRouterKey string `json:"openRouterKey"`
	DropboxPath   string `json:"dropboxPath"`
	Model         string `json:"model"`
}
type Document struct {
	ID, ProjectID, Name, Path, CreatedAt, Status, Category, SuggestedPath, Reason string `json:",omitempty"`
	Size                                                                          int64  `json:"size"`
	Confidence                                                                    int    `json:"confidence"`
}
type State struct {
	Settings  Settings   `json:"settings"`
	Documents []Document `json:"documents"`
}
type App struct {
	mu             sync.Mutex
	root, dataFile string
	state          State
	ln             net.Listener
}

var categories = []string{"01_ADMINISTRATIVO", "02_PROJETOS", "03_FISCALIZACAO", "04_ATAS", "05_AUTOS", "06_PLANEAMENTO", "07_SEGURANCA", "08_ENSAIOS", "09_FICHAS_TECNICAS", "10_COMUNICACOES", "11_FOTOGRAFIAS", "12_TRABALHOS_ADICIONAIS", "13_LIVRO_OBRA", "14_FECHO_OBRA", "99_POR_VALIDAR"}

func dataDir() string {
	if p := os.Getenv("LOCALAPPDATA"); p != "" {
		return filepath.Join(p, "DirecaoFiscalizacaoObra")
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, "DirecaoFiscalizacaoObra")
}
func newApp() *App {
	r := dataDir()
	_ = os.MkdirAll(r, 0755)
	a := &App{root: r, dataFile: filepath.Join(r, "app.json"), state: State{Settings: Settings{Model: "openai/gpt-5-mini"}, Documents: []Document{}}}
	if b, e := os.ReadFile(a.dataFile); e == nil {
		_ = json.Unmarshal(b, &a.state)
	}
	a.save()
	return a
}
func (a *App) save() {
	b, _ := json.MarshalIndent(a.state, "", "  ")
	_ = os.WriteFile(a.dataFile, b, 0644)
}
func send(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writable(p string) (bool, string) {
	st, e := os.Stat(p)
	if e != nil {
		return false, e.Error()
	}
	if !st.IsDir() {
		return false, "O caminho não é uma pasta."
	}
	t := filepath.Join(p, fmt.Sprintf(".dfobra_%d.tmp", time.Now().UnixNano()))
	if e = os.WriteFile(t, []byte("ok"), 0644); e != nil {
		return false, e.Error()
	}
	_ = os.Remove(t)
	return true, ""
}
func detectDropbox() string {
	h, _ := os.UserHomeDir()
	for _, p := range []string{filepath.Join(h, "Dropbox"), filepath.Join(h, "Dropbox (Personal)")} {
		if st, e := os.Stat(p); e == nil && st.IsDir() {
			return p
		}
	}
	if la := os.Getenv("LOCALAPPDATA"); la != "" {
		b, e := os.ReadFile(filepath.Join(la, "Dropbox", "info.json"))
		if e == nil {
			var x map[string]map[string]any
			if json.Unmarshal(b, &x) == nil {
				for _, v := range x {
					if p, ok := v["path"].(string); ok {
						if st, e := os.Stat(p); e == nil && st.IsDir() {
							return p
						}
					}
				}
			}
		}
	}
	return ""
}
func request(method, url, key string, payload any) (map[string]any, error) {
	var body io.Reader
	if payload != nil {
		b, _ := json.Marshal(payload)
		body = bytes.NewReader(b)
	}
	req, e := http.NewRequest(method, url, body)
	if e != nil {
		return nil, e
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c := &http.Client{Timeout: 50 * time.Second}
	r, e := c.Do(req)
	if e != nil {
		return nil, e
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(r.Body, 6<<20))
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	if r.StatusCode < 200 || r.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", r.StatusCode, string(b))
	}
	return out, nil
}
func inboxRoot(db string) string {
	if db != "" {
		return filepath.Join(db, "DirecaoFiscalizacaoObra", "IB1", "00_INBOX_IA")
	}
	return filepath.Join(dataDir(), "inbox", "IB1")
}
func projectRoot(db string) string {
	if db != "" {
		return filepath.Join(db, "DirecaoFiscalizacaoObra", "IB1")
	}
	return filepath.Join(dataDir(), "arquivo", "IB1")
}
func ensureStructure(root string) {
	_ = os.MkdirAll(filepath.Join(root, "00_INBOX_IA"), 0755)
	for _, c := range categories {
		_ = os.MkdirAll(filepath.Join(root, c), 0755)
	}
}
func cleanName(s string) string {
	return strings.ToLower(strings.NewReplacer("á", "a", "à", "a", "ã", "a", "â", "a", "é", "e", "ê", "e", "í", "i", "ó", "o", "ô", "o", "õ", "o", "ú", "u", "ç", "c").Replace(s))
}
func classifyHeuristic(name string) (string, int, string) {
	n := cleanName(name)
	ext := strings.ToLower(filepath.Ext(n))
	checks := []struct {
		cat    string
		score  int
		keys   []string
		reason string
	}{
		{"04_ATAS", 95, []string{"ata ", "ata_", "ata-", "reuniao", "reunião", "minuta"}, "Nome indica ata/reunião."},
		{"05_AUTOS", 95, []string{"auto ", "auto_", "auto-", "medicao", "medição", "mapa de medicao"}, "Nome indica auto ou medição."},
		{"06_PLANEAMENTO", 93, []string{"planeamento", "cronograma", "planning", "programa de trabalhos", "gantt"}, "Nome indica planeamento/cronograma."},
		{"13_LIVRO_OBRA", 95, []string{"livro de obra", "livro_obra", "livro-obra"}, "Nome indica Livro de Obra."},
		{"07_SEGURANCA", 92, []string{"pss", "seguranca", "segurança", "cso", "coordenador de seguranca", "acidente", "ficha de procedimento"}, "Nome indica segurança e saúde."},
		{"08_ENSAIOS", 92, []string{"ensaio", "provet", "laboratorio", "laboratório", "teste betao", "betão", "betao", "guia betao", "guia_betao"}, "Nome indica ensaio/betão/laboratório."},
		{"09_FICHAS_TECNICAS", 92, []string{"ficha tecnica", "ficha_t", "datasheet", "declaracao desempenho", "dop", "certificado produto"}, "Nome indica ficha técnica/certificação de produto."},
		{"12_TRABALHOS_ADICIONAIS", 90, []string{"trabalho a mais", "trabalhos a mais", "tnp", "adicional", "extra"}, "Nome indica trabalho adicional/não previsto."},
		{"10_COMUNICACOES", 86, []string{"email", "e-mail", "oficio", "ofício", "carta", "comunicacao", "comunicação", "whatsapp"}, "Nome indica comunicação."},
		{"14_FECHO_OBRA", 90, []string{"rececao provisoria", "receção provisória", "rececao definitiva", "fecho", "telas finais", "garantia"}, "Nome indica fecho/receção da obra."},
		{"01_ADMINISTRATIVO", 88, []string{"termo", "seguro", "alvara", "alvará", "impic", "contrato", "adjudicacao", "adjudicação", "licenca", "licença"}, "Nome indica documento administrativo/legal."},
		{"02_PROJETOS", 88, []string{"projeto", "projecto", "arquitetura", "arquitectura", "estrutura", "estabilidade", "avac", "eletricidade", "aguas", "águas", "esgotos", "desenho", "planta", "corte", "alçado", "alcado", "rev."}, "Nome indica projeto ou peça desenhada."},
		{"03_FISCALIZACAO", 84, []string{"fiscalizacao", "fiscalização", "relatorio", "relatório", "visita", "ocorrencia", "ocorrência", "nao conformidade", "não conformidade", "diretiva"}, "Nome indica registo de fiscalização."},
	}
	for _, c := range checks {
		for _, k := range c.keys {
			if strings.Contains(n, cleanName(k)) {
				return c.cat, c.score, c.reason
			}
		}
	}
	if ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".webp" || ext == ".heic" || ext == ".bmp" {
		return "11_FOTOGRAFIAS", 90, "Extensão de imagem/fotografia."
	}
	return "99_POR_VALIDAR", 45, "Não foi possível classificar com segurança apenas pelo nome/tipo."
}
func aiClassify(key, model, name string) (string, int, string) {
	if key == "" {
		return classifyHeuristic(name)
	}
	prompt := fmt.Sprintf(`Classifica o ficheiro de uma obra de construção chamado %q numa destas categorias exatas: %s. Responde APENAS JSON: {"category":"...","confidence":0-100,"reason":"..."}. Não inventes conteúdo que não esteja no nome do ficheiro.`, name, strings.Join(categories, ", "))
	out, e := request("POST", "https://openrouter.ai/api/v1/chat/completions", key, map[string]any{"model": model, "messages": []map[string]string{{"role": "user", "content": prompt}}, "temperature": 0})
	if e != nil {
		return classifyHeuristic(name)
	}
	text := ""
	if c, ok := out["choices"].([]any); ok && len(c) > 0 {
		if x, ok := c[0].(map[string]any); ok {
			if mm, ok := x["message"].(map[string]any); ok {
				text, _ = mm["content"].(string)
			}
		}
	}
	text = strings.TrimSpace(strings.Trim(text, "`"))
	if i := strings.Index(text, "{"); i >= 0 {
		text = text[i:]
	}
	if j := strings.LastIndex(text, "}"); j >= 0 {
		text = text[:j+1]
	}
	var v struct {
		Category   string `json:"category"`
		Confidence int    `json:"confidence"`
		Reason     string `json:"reason"`
	}
	if json.Unmarshal([]byte(text), &v) == nil {
		for _, c := range categories {
			if v.Category == c {
				if v.Confidence < 0 {
					v.Confidence = 0
				}
				if v.Confidence > 100 {
					v.Confidence = 100
				}
				return v.Category, v.Confidence, v.Reason
			}
		}
	}
	return classifyHeuristic(name)
}
func findDocByPath(docs []Document, p string) int {
	for i := range docs {
		if strings.EqualFold(docs[i].Path, p) {
			return i
		}
	}
	return -1
}
func uniqueDest(dir, name string) string {
	d := filepath.Join(dir, name)
	if _, e := os.Stat(d); os.IsNotExist(e) {
		return d
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 2; i < 10000; i++ {
		x := filepath.Join(dir, fmt.Sprintf("%s (%d)%s", base, i, ext))
		if _, e := os.Stat(x); os.IsNotExist(e) {
			return x
		}
	}
	return filepath.Join(dir, fmt.Sprintf("%d_%s", time.Now().UnixNano(), name))
}
func (a *App) scanInbox(analyze bool) ([]Document, error) {
	a.mu.Lock()
	db := a.state.Settings.DropboxPath
	key := a.state.Settings.OpenRouterKey
	model := a.state.Settings.Model
	a.mu.Unlock()
	root := inboxRoot(db)
	pr := projectRoot(db)
	ensureStructure(pr)
	_ = os.MkdirAll(root, 0755)
	entries, e := os.ReadDir(root)
	if e != nil {
		return nil, e
	}
	var found []Document
	for _, en := range entries {
		if en.IsDir() {
			continue
		}
		p := filepath.Join(root, en.Name())
		info, e := en.Info()
		if e != nil {
			continue
		}
		cat, conf, reason := "", 0, ""
		status := "Recebido"
		suggest := ""
		if analyze {
			cat, conf, reason = aiClassify(key, model, en.Name())
			suggest = filepath.Join(pr, cat, en.Name())
			status = "Analisado"
			if conf < 70 {
				cat = "99_POR_VALIDAR"
				suggest = filepath.Join(pr, cat, en.Name())
				status = "Para validar"
			}
		}
		d := Document{ID: fmt.Sprintf("DOC-%d", time.Now().UnixNano()), ProjectID: "IB1", Name: en.Name(), Path: p, Size: info.Size(), CreatedAt: info.ModTime().Format(time.RFC3339), Status: status, Category: cat, Confidence: conf, SuggestedPath: suggest, Reason: reason}
		a.mu.Lock()
		idx := findDocByPath(a.state.Documents, p)
		if idx >= 0 {
			old := a.state.Documents[idx]
			d.ID = old.ID
			if !analyze && old.Status != "" {
				d = old
			}
			a.state.Documents[idx] = d
		} else {
			a.state.Documents = append([]Document{d}, a.state.Documents...)
		}
		a.save()
		a.mu.Unlock()
		found = append(found, d)
	}
	return found, nil
}
func (a *App) organizeAnalyzed() (int, int, error) {
	a.mu.Lock()
	db := a.state.Settings.DropboxPath
	docs := append([]Document(nil), a.state.Documents...)
	a.mu.Unlock()
	pr := projectRoot(db)
	ensureStructure(pr)
	moved, review := 0, 0
	for i := range docs {
		d := &docs[i]
		if d.ProjectID != "IB1" || d.Status == "Organizado" {
			continue
		}
		if d.Category == "" || d.Confidence < 70 || d.Category == "99_POR_VALIDAR" {
			review++
			continue
		}
		if _, e := os.Stat(d.Path); e != nil {
			continue
		}
		dir := filepath.Join(pr, d.Category)
		_ = os.MkdirAll(dir, 0755)
		dest := uniqueDest(dir, d.Name)
		if e := os.Rename(d.Path, dest); e != nil {
			src, e2 := os.Open(d.Path)
			if e2 != nil {
				continue
			}
			dst, e3 := os.Create(dest)
			if e3 != nil {
				src.Close()
				continue
			}
			_, e4 := io.Copy(dst, src)
			dst.Close()
			src.Close()
			if e4 != nil {
				_ = os.Remove(dest)
				continue
			}
			_ = os.Remove(d.Path)
		}
		oldPath := d.Path
		d.Path = dest
		d.SuggestedPath = dest
		d.Status = "Organizado"
		moved++
		a.mu.Lock()
		idx := findDocByPath(a.state.Documents, oldPath)
		if idx >= 0 {
			a.state.Documents[idx] = *d
		}
		a.save()
		a.mu.Unlock()
	}
	return moved, review, nil
}

func (a *App) api(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if p == "/api/health" {
		send(w, 200, map[string]any{"ok": true, "version": "5.0.0"})
		return
	}
	if p == "/api/state" {
		a.mu.Lock()
		defer a.mu.Unlock()
		docs := append([]Document(nil), a.state.Documents...)
		sort.SliceStable(docs, func(i, j int) bool { return docs[i].CreatedAt > docs[j].CreatedAt })
		send(w, 200, map[string]any{"settings": map[string]any{"openRouterKey": func() string {
			if a.state.Settings.OpenRouterKey != "" {
				return "***"
			}
			return ""
		}(), "dropboxPath": a.state.Settings.DropboxPath}, "documents": docs, "connections": map[string]bool{"openrouter": a.state.Settings.OpenRouterKey != "", "dropbox": a.state.Settings.DropboxPath != ""}})
		return
	}
	if p == "/api/settings/openrouter" && r.Method == "POST" {
		var b struct {
			Key string `json:"key"`
		}
		if json.NewDecoder(r.Body).Decode(&b) != nil || len(strings.TrimSpace(b.Key)) < 10 {
			send(w, 400, map[string]any{"error": "Chave inválida."})
			return
		}
		a.mu.Lock()
		a.state.Settings.OpenRouterKey = strings.TrimSpace(b.Key)
		a.save()
		a.mu.Unlock()
		send(w, 200, map[string]any{"ok": true})
		return
	}
	if p == "/api/openrouter/test" && r.Method == "POST" {
		a.mu.Lock()
		k := a.state.Settings.OpenRouterKey
		a.mu.Unlock()
		if k == "" {
			send(w, 400, map[string]any{"error": "Chave OpenRouter não configurada."})
			return
		}
		_, e := request("GET", "https://openrouter.ai/api/v1/models", k, nil)
		if e != nil {
			send(w, 502, map[string]any{"error": e.Error()})
			return
		}
		send(w, 200, map[string]any{"ok": true})
		return
	}
	if p == "/api/ask" && r.Method == "POST" {
		var b struct {
			Prompt string `json:"prompt"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		a.mu.Lock()
		k := a.state.Settings.OpenRouterKey
		m := a.state.Settings.Model
		a.mu.Unlock()
		if k == "" {
			send(w, 400, map[string]any{"error": "Chave OpenRouter não configurada."})
			return
		}
		out, e := request("POST", "https://openrouter.ai/api/v1/chat/completions", k, map[string]any{"model": m, "messages": []map[string]string{{"role": "system", "content": "És o assistente da plataforma Direção e Fiscalização de Obra. Responde em português de Portugal. Sê proativo, objetivo e orientado para prazo, custo, qualidade, segurança, documentação e antecipação de risco."}, {"role": "user", "content": b.Prompt}}, "temperature": 0.2})
		if e != nil {
			send(w, 502, map[string]any{"error": e.Error()})
			return
		}
		text := "Sem resposta."
		if c, ok := out["choices"].([]any); ok && len(c) > 0 {
			if x, ok := c[0].(map[string]any); ok {
				if mm, ok := x["message"].(map[string]any); ok {
					if t, ok := mm["content"].(string); ok {
						text = t
					}
				}
			}
		}
		send(w, 200, map[string]any{"ok": true, "text": text})
		return
	}
	if p == "/api/dropbox/detect" && r.Method == "POST" {
		d := detectDropbox()
		if d == "" {
			send(w, 404, map[string]any{"error": "Dropbox não encontrada automaticamente."})
			return
		}
		a.mu.Lock()
		a.state.Settings.DropboxPath = d
		a.save()
		a.mu.Unlock()
		ensureStructure(projectRoot(d))
		send(w, 200, map[string]any{"ok": true, "path": d})
		return
	}
	if p == "/api/dropbox/set" && r.Method == "POST" {
		var b struct {
			Path string `json:"path"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		pp := strings.TrimSpace(b.Path)
		ok, msg := writable(pp)
		if !ok {
			send(w, 400, map[string]any{"error": msg})
			return
		}
		a.mu.Lock()
		a.state.Settings.DropboxPath = pp
		a.save()
		a.mu.Unlock()
		ensureStructure(projectRoot(pp))
		send(w, 200, map[string]any{"ok": true, "path": pp})
		return
	}
	if p == "/api/dropbox/browse" && r.Method == "POST" {
		script := `Add-Type -AssemblyName System.Windows.Forms; $f=New-Object System.Windows.Forms.FolderBrowserDialog; $f.Description='Escolha a pasta principal da Dropbox'; if($f.ShowDialog() -eq 'OK'){[Console]::OutputEncoding=[Text.UTF8Encoding]::UTF8; Write-Output $f.SelectedPath}`
		out, e := exec.Command("powershell.exe", "-NoProfile", "-STA", "-Command", script).Output()
		if e != nil || strings.TrimSpace(string(out)) == "" {
			send(w, 400, map[string]any{"error": "Nenhuma pasta selecionada."})
			return
		}
		pp := strings.TrimSpace(string(out))
		ok, msg := writable(pp)
		if !ok {
			send(w, 400, map[string]any{"error": msg})
			return
		}
		a.mu.Lock()
		a.state.Settings.DropboxPath = pp
		a.save()
		a.mu.Unlock()
		ensureStructure(projectRoot(pp))
		send(w, 200, map[string]any{"ok": true, "path": pp})
		return
	}
	if p == "/api/dropbox/scan" && r.Method == "POST" {
		docs, e := a.scanInbox(false)
		if e != nil {
			send(w, 500, map[string]any{"error": e.Error()})
			return
		}
		send(w, 200, map[string]any{"ok": true, "found": len(docs)})
		return
	}
	if p == "/api/dropbox/analyze" && r.Method == "POST" {
		docs, e := a.scanInbox(true)
		if e != nil {
			send(w, 500, map[string]any{"error": e.Error()})
			return
		}
		review := 0
		for _, d := range docs {
			if d.Status == "Para validar" {
				review++
			}
		}
		send(w, 200, map[string]any{"ok": true, "analyzed": len(docs), "review": review})
		return
	}
	if p == "/api/dropbox/organize" && r.Method == "POST" {
		m, rv, e := a.organizeAnalyzed()
		if e != nil {
			send(w, 500, map[string]any{"error": e.Error()})
			return
		}
		send(w, 200, map[string]any{"ok": true, "moved": m, "review": rv})
		return
	}
	if p == "/api/upload" && r.Method == "PUT" {
		name := filepath.Base(r.URL.Query().Get("name"))
		if name == "." || name == "" {
			name = "ficheiro"
		}
		a.mu.Lock()
		db := a.state.Settings.DropboxPath
		a.mu.Unlock()
		root := inboxRoot(db)
		_ = os.MkdirAll(root, 0755)
		dest := uniqueDest(root, name)
		f, e := os.Create(dest)
		if e != nil {
			send(w, 500, map[string]any{"error": e.Error()})
			return
		}
		n, e := io.Copy(f, io.LimitReader(r.Body, 200<<20))
		f.Close()
		if e != nil {
			send(w, 500, map[string]any{"error": e.Error()})
			return
		}
		d := Document{ID: fmt.Sprintf("DOC-%d", time.Now().UnixNano()), ProjectID: "IB1", Name: filepath.Base(dest), Path: dest, Size: n, CreatedAt: time.Now().Format(time.RFC3339), Status: "Recebido"}
		a.mu.Lock()
		a.state.Documents = append([]Document{d}, a.state.Documents...)
		a.save()
		a.mu.Unlock()
		send(w, 200, map[string]any{"ok": true})
		return
	}
	send(w, 404, map[string]any{"error": "Endpoint inexistente"})
}
func (a *App) handler(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		a.api(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, indexHTML)
}
func openBrowser(u string) { _ = exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start() }
func main() {
	if runtime.GOOS != "windows" {
	}
	a := newApp()
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		return
	}
	a.ln = ln
	u := "http://127.0.0.1:" + fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)
	go http.Serve(ln, http.HandlerFunc(a.handler))
	time.Sleep(300 * time.Millisecond)
	openBrowser(u)
	select {}
}

const indexHTML = `<!doctype html><html lang="pt"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Direção e Fiscalização de Obra</title><style>body{margin:0;font-family:Segoe UI,Arial;background:#f4f7f8;color:#17212b}.wrap{max-width:1180px;margin:auto;padding:24px}.top{display:flex;justify-content:space-between;align-items:center}.card{background:white;border:1px solid #dce5e8;border-radius:16px;padding:18px;margin:14px 0}.grid{display:grid;grid-template-columns:repeat(4,1fr);gap:12px}.metric b{font-size:26px}.two{display:grid;grid-template-columns:1.35fr 1fr;gap:14px}.btn{padding:11px 14px;border-radius:10px;border:1px solid #b9c8cd;background:white;cursor:pointer}.btn:disabled{opacity:.45;cursor:not-allowed}.primary{background:#0d8a84;color:white;border-color:#0d8a84}.drop{border:2px dashed #9fb4bb;border-radius:14px;padding:26px;text-align:center}.good{color:#137333}.bad{color:#c52b2b}.warn{color:#a05a00}textarea,input{width:100%;box-sizing:border-box;padding:12px;border:1px solid #cad5da;border-radius:10px;font-size:16px}textarea{min-height:110px}.row{display:flex;gap:8px;flex-wrap:wrap}.answer{white-space:pre-wrap;background:#f6fafb;border-radius:10px;padding:12px;margin-top:10px}.toolbar{display:flex;gap:10px;flex-wrap:wrap;margin:14px 0}.doc{display:grid;grid-template-columns:1.5fr .8fr .6fr 2fr;gap:10px;align-items:center;border-top:1px solid #e7edef;padding:10px 0;font-size:14px}.pill{display:inline-block;border:1px solid #d2dde1;border-radius:999px;padding:4px 8px;background:#f8fbfc}.small{font-size:13px;color:#62717a}.status{margin-top:10px;padding:10px;border-radius:10px;background:#f6fafb}@media(max-width:850px){.grid,.two{grid-template-columns:1fr 1fr}.doc{grid-template-columns:1fr}}@media(max-width:560px){.grid,.two{grid-template-columns:1fr}}</style></head><body><div class="wrap"><div class="top"><div><h1>Direção e Fiscalização de Obra</h1><div>IB1 — Espinho · obra-piloto</div></div></div><div class="grid"><div class="card metric">Prazo<br><b>—</b></div><div class="card metric">Execução física<br><b>—</b></div><div class="card metric">Execução financeira<br><b>—</b></div><div class="card metric">Riscos<br><b>0</b></div></div><div class="two"><div class="card"><h2>Assistente IA</h2><div class="row"><button class="btn quick">Como está a obra?</button><button class="btn quick">Riscos 7 dias</button><button class="btn quick">O que falta validar?</button></div><textarea id="prompt" placeholder="Pergunte sobre a obra..."></textarea><button class="btn primary" id="ask">Analisar</button><div id="answer" class="answer" style="display:none"></div></div><div class="card"><h2>Ligações</h2><p>OpenRouter: <b id="or">Por configurar</b></p><p>Dropbox: <b id="db">Por configurar</b></p><input id="key" type="password" placeholder="Chave OpenRouter"><div class="row"><button class="btn" id="savekey">Guardar chave</button><button class="btn" id="testkey">Testar</button></div><br><input id="path" placeholder="C:\Users\Nome\Dropbox"><div class="row"><button class="btn" id="savepath">Guardar caminho</button><button class="btn" id="browse">Escolher pasta</button><button class="btn" id="detect">Detetar</button></div><p id="msg"></p></div></div><div class="card"><h2>Arquivo Vivo</h2><div class="drop" id="drop"><b>Largue aqui documentos ou fotografias</b><br><span class="small">Ou coloque-os diretamente na Dropbox da obra</span><br><br><input id="files" type="file" multiple><p id="up"></p></div><div class="toolbar"><button class="btn" id="scan">1. Ler Caixa Dropbox</button><button class="btn primary" id="analyze">2. Analisar e Classificar</button><button class="btn" id="organize">3. Organizar automaticamente</button></div><div class="status" id="archiveStatus">Aguardamos documentos.</div><div id="docs"></div></div></div><script>const $=s=>document.querySelector(s);const esc=s=>String(s??'').replace(/[&<>\"]/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]));async function api(u,o={}){const r=await fetch(u,o);let j={};try{j=await r.json()}catch{}if(!r.ok)throw Error(j.error||('HTTP '+r.status));return j}async function refresh(){try{const s=await api('/api/state');$('#or').textContent=s.connections.openrouter?'Ligado':'Por configurar';$('#or').className=s.connections.openrouter?'good':'bad';$('#db').textContent=s.connections.dropbox?'Ligada':'Por configurar';$('#db').className=s.connections.dropbox?'good':'bad';$('#path').value=s.settings.dropboxPath||'';const docs=s.documents||[];$('#docs').innerHTML=docs.slice(0,80).map(d=>'<div class="doc"><div><b>'+esc(d.Name)+'</b><div class="small">'+esc(d.Reason||'')+'</div></div><div><span class="pill">'+esc(d.Category||'Sem classificação')+'</span></div><div><b>'+esc(d.Confidence||0)+'%</b><br><span class="small">'+esc(d.Status)+'</span></div><div class="small">'+esc(d.SuggestedPath||d.Path||'')+'</div></div>').join('');let rev=docs.filter(d=>d.Status==='Para validar').length;let ana=docs.filter(d=>d.Status==='Analisado').length;let org=docs.filter(d=>d.Status==='Organizado').length;$('#archiveStatus').textContent=docs.length?docs.length+' documentos registados · '+ana+' analisados · '+org+' organizados · '+rev+' para validar':'Aguardamos documentos.'}catch(e){$('#msg').textContent=e.message}}$('#savekey').onclick=async()=>{try{await api('/api/settings/openrouter',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({key:$('#key').value})});$('#msg').textContent='Chave guardada.';refresh()}catch(e){$('#msg').textContent=e.message}};$('#testkey').onclick=async()=>{try{$('#msg').textContent='A testar...';await api('/api/openrouter/test',{method:'POST'});$('#msg').textContent='OpenRouter OK.'}catch(e){$('#msg').textContent=e.message}};$('#savepath').onclick=async()=>{try{const j=await api('/api/dropbox/set',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({path:$('#path').value})});$('#msg').textContent='Dropbox: '+j.path;refresh()}catch(e){$('#msg').textContent=e.message}};$('#browse').onclick=async()=>{try{const j=await api('/api/dropbox/browse',{method:'POST'});$('#path').value=j.path;$('#msg').textContent='Dropbox: '+j.path;refresh()}catch(e){$('#msg').textContent=e.message}};$('#detect').onclick=async()=>{try{const j=await api('/api/dropbox/detect',{method:'POST'});$('#path').value=j.path;$('#msg').textContent='Dropbox encontrada.';refresh()}catch(e){$('#msg').textContent=e.message}};async function upload(fs){let n=0;for(const f of fs){$('#up').textContent='A enviar '+f.name+'...';await api('/api/upload?name='+encodeURIComponent(f.name),{method:'PUT',body:f});n++}$('#up').textContent=n+' ficheiro(s) recebido(s). Pode agora analisar e organizar.';refresh()}$('#files').onchange=e=>upload(e.target.files);['dragenter','dragover'].forEach(x=>$('#drop').addEventListener(x,e=>{e.preventDefault();$('#drop').style.background='#eef8f7'}));['dragleave','drop'].forEach(x=>$('#drop').addEventListener(x,e=>{e.preventDefault();$('#drop').style.background=''}));$('#drop').addEventListener('drop',e=>upload(e.dataTransfer.files));$('#scan').onclick=async()=>{try{$('#archiveStatus').textContent='A ler a Caixa Dropbox...';const j=await api('/api/dropbox/scan',{method:'POST'});$('#archiveStatus').textContent='Encontrados '+j.found+' ficheiro(s) na Caixa de Entrada.';refresh()}catch(e){$('#archiveStatus').textContent=e.message}};$('#analyze').onclick=async()=>{try{$('#archiveStatus').textContent='A analisar e classificar documentos...';const j=await api('/api/dropbox/analyze',{method:'POST'});$('#archiveStatus').textContent='Analisados '+j.analyzed+' · '+j.review+' precisam de validação.';refresh()}catch(e){$('#archiveStatus').textContent=e.message}};$('#organize').onclick=async()=>{try{if(!confirm('Organizar automaticamente os documentos com confiança suficiente? Os casos duvidosos ficam em Para Validar.'))return;$('#archiveStatus').textContent='A organizar...';const j=await api('/api/dropbox/organize',{method:'POST'});$('#archiveStatus').textContent='Organizados '+j.moved+' · '+j.review+' ficaram para validar.';refresh()}catch(e){$('#archiveStatus').textContent=e.message}};$('#ask').onclick=async()=>{try{$('#answer').style.display='block';$('#answer').textContent='A analisar...';const j=await api('/api/ask',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({prompt:$('#prompt').value})});$('#answer').textContent=j.text}catch(e){$('#answer').textContent=e.message}};document.querySelectorAll('.quick').forEach(b=>b.onclick=()=>{$('#prompt').value=b.textContent;$('#ask').click()});refresh();</script></body></html>`
