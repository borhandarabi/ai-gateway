package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------
// تنظیمات
// ---------------------------------------------------------------------
var (
	templateFile = getEnvDefault("TEMPLATE_FILE", "template.json")
	nodesFile    = getEnvDefault("NODES_FILE", "nodes.json")
	configFile   = getEnvDefault("CONFIG_FILE", "config.json")
	stateFile    = getEnvDefault("STATE_FILE", "state.json") // متادیتای داخلی خود مدیر (نه چیزی که sing-box می‌خواند)
)

const (
	singBoxCheckTimeout = 30 * time.Second
	singBoxStopTimeout  = 10 * time.Second
	warpHTTPTimeout     = 15 * time.Second
)

var (
	// آدرس bind شدن مدیریت (پیش‌فرض: فقط لوکال‌هاست، برای امنیت)
	bindAddr = getEnvDefault("BIND_ADDR", "127.0.0.1")
	apiPort  = ":" + getEnvDefault("API_PORT", "5000")

	// توکن مدیریتی اختیاری. اگر خالی باشد API بدون احراز هویت است (فقط برای dev/local).
	// می‌تواند بعداً از داخل UI (صفحه‌ی Settings) نیز تغییر و در state.json ذخیره شود.
	adminToken   = os.Getenv("ADMIN_TOKEN")
	adminTokenMu sync.RWMutex

	// قفل عملیات فایل/کانفیگ (read-write): نوشتن‌ها Lock می‌گیرند، خواندن‌ها RLock.
	mu sync.RWMutex

	serviceNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
	warpTagRe     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)
)

func getEnvDefault(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// managedEnvKeys لیست کلیدهای مبتنی‌بر env هستند که از صفحه‌ی Settings هم قابل
// مدیریت‌اند (نه فقط با export کردن قبل از اجرا).
var managedEnvKeys = []string{
	"BIND_ADDR", "API_PORT",
	"SINGBOX_PATH", "SINGBOX_VERSION", "SINGBOX_INSTALL_DIR", "SINGBOX_NO_AUTO_DOWNLOAD",
	"CLOUDFLARED_PATH", "CLOUDFLARED_INSTALL_DIR",
}

// envKeysRequiringRestart کلیدهایی هستند که چون روی socket شنود HTTP یا متغیرهای
// سراسری خوانده‌شده در ابتدای main() اثر می‌گذارند، فقط از ری‌استارت بعدی مدیر اعمال می‌شوند.
var envKeysRequiringRestart = map[string]bool{"BIND_ADDR": true, "API_PORT": true}

var (
	envOverridesMu    sync.RWMutex
	envOverridesCache map[string]string
)

// loadEnvOverridesCache باید در ابتدای main() صدا زده شود تا override های ذخیره‌شده
// از UI، قبل از هر استفاده‌ای از getSetting، در حافظه بارگذاری شوند.
func loadEnvOverridesCache() {
	state := readStateOrDefault()
	envOverridesMu.Lock()
	envOverridesCache = state.EnvOverrides
	if envOverridesCache == nil {
		envOverridesCache = map[string]string{}
	}
	envOverridesMu.Unlock()
}

// getSetting یک تنظیم را با این اولویت برمی‌گرداند: مقدار ذخیره‌شده از صفحه‌ی
// Settings (state.json) > متغیر محیطی واقعی > مقدار پیش‌فرض. یعنی تغییر از UI
// واقعاً روی رفتار مدیر اثر می‌گذارد، نه فقط ذخیره‌ی نمایشی.
func getSetting(key, def string) string {
	envOverridesMu.RLock()
	v, ok := envOverridesCache[key]
	envOverridesMu.RUnlock()
	if ok && v != "" {
		return v
	}
	return getEnvDefault(key, def)
}

// setSetting یک override را در state.json ذخیره می‌کند (مقدار خالی یعنی حذف
// override و بازگشت به متغیر محیطی/پیش‌فرض).
func setSetting(key, value string) error {
	state := readStateOrDefault()
	if state.EnvOverrides == nil {
		state.EnvOverrides = map[string]string{}
	}
	value = strings.TrimSpace(value)
	if value == "" {
		delete(state.EnvOverrides, key)
	} else {
		state.EnvOverrides[key] = value
	}
	if err := writeState(state); err != nil {
		return err
	}
	envOverridesMu.Lock()
	envOverridesCache = state.EnvOverrides
	envOverridesMu.Unlock()
	return nil
}

func jsonResponse(w http.ResponseWriter, status int, data map[string]interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

// requireMethod مطمئن می‌شود که هندلر فقط با متد مشخص‌شده صدا زده می‌شود.
func requireMethod(method string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			jsonResponse(w, http.StatusMethodNotAllowed, map[string]interface{}{"error": "Method not allowed"})
			return
		}
		next(w, r)
	}
}

// settingsMethodRouter یک مسیر واحد را بین دو هندلر بر اساس متد HTTP تقسیم می‌کند
// (GET برای خواندن وضعیت فعلی، POST برای ذخیره‌ی تغییرات) — برای /api/cloudflare/settings.
func settingsMethodRouter(getHandler, postHandler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			getHandler(w, r)
		case http.MethodPost:
			postHandler(w, r)
		default:
			jsonResponse(w, http.StatusMethodNotAllowed, map[string]interface{}{"error": "Method not allowed"})
		}
	}
}

// requireAuth در صورتی که ADMIN_TOKEN تنظیم شده باشد، هدر X-Admin-Token (یا query param token) را بررسی می‌کند.
func requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if tok := getAdminToken(); tok != "" {
			got := r.Header.Get("X-Admin-Token")
			if got == "" {
				got = r.URL.Query().Get("token")
			}
			if got != tok {
				jsonResponse(w, http.StatusUnauthorized, map[string]interface{}{"error": "Unauthorized"})
				return
			}
		}
		next(w, r)
	}
}

// getAdminToken/setAdminToken دسترسی امن (thread-safe) به توکن مدیریتی را فراهم می‌کنند
// تا هم درخواست‌های همزمان و هم تغییر آن از صفحه‌ی Settings بدون data race باشد.
func getAdminToken() string {
	adminTokenMu.RLock()
	defer adminTokenMu.RUnlock()
	return adminToken
}

// setAdminToken توکن را در حافظه به‌روزرسانی و در state.json ذخیره می‌کند تا پس از
// ری‌استارت مدیر نیز باقی بماند (و بر متغیر محیطی ADMIN_TOKEN اولویت پیدا کند).
func setAdminToken(token string) error {
	adminTokenMu.Lock()
	adminToken = token
	adminTokenMu.Unlock()

	state := readStateOrDefault()
	state.AdminToken = token
	return writeState(state)
}

const htmlContent = `<!DOCTYPE html>
<html lang="en" dir="ltr">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>sb::manager</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link href="https://fonts.googleapis.com/css2?family=Space+Grotesk:wght@500;600;700&family=Inter:wght@400;500;600;700&family=JetBrains+Mono:wght@400;500;600&display=swap" rel="stylesheet">
<style>
  :root{
    --bg:#0a0d13;
    --surface:#11151f;
    --surface-2:#161c29;
    --surface-hover:#1b2231;
    --border:#212938;
    --border-soft:#1a2130;
    --text:#e8ebf3;
    --text-muted:#8b93a8;
    --text-dim:#565f74;
    --accent:#5b8cff;
    --accent-soft:rgba(91,140,255,.13);
    --accent-strong:#7ea2ff;
    --success:#34d399;
    --success-soft:rgba(52,211,153,.13);
    --danger:#f0546b;
    --danger-soft:rgba(240,84,107,.13);
    --warning:#f5a623;
    --warning-soft:rgba(245,166,35,.13);
    --radius:10px;
    --radius-sm:7px;
    --font-display:'Space Grotesk',ui-sans-serif,sans-serif;
    --font-body:'Inter',ui-sans-serif,sans-serif;
    --font-mono:'JetBrains Mono',ui-monospace,monospace;
  }
  *{box-sizing:border-box;}
  html,body{margin:0;padding:0;}
  body{
    background:var(--bg);
    color:var(--text);
    font-family:var(--font-body);
    font-size:14px;
    line-height:1.5;
    -webkit-font-smoothing:antialiased;
  }
  ::selection{background:var(--accent-soft);color:var(--text);}
  a{color:var(--accent-strong);}
  ::-webkit-scrollbar{width:10px;height:10px;}
  ::-webkit-scrollbar-track{background:transparent;}
  ::-webkit-scrollbar-thumb{background:var(--border);border-radius:8px;}
  ::-webkit-scrollbar-thumb:hover{background:var(--text-dim);}

  button{font-family:inherit;cursor:pointer;}
  input,select,textarea{font-family:inherit;}

  /* ---------- layout shell ---------- */
  #shell{display:flex;min-height:100vh;}
  #sidebar{
    width:236px;flex:none;
    background:var(--surface);
    border-right:1px solid var(--border-soft);
    display:flex;flex-direction:column;
    padding:20px 14px;
    position:sticky;top:0;height:100vh;
  }
  .brand{display:flex;align-items:center;gap:10px;padding:6px 8px 22px 8px;}
  .brand-mark{
    width:30px;height:30px;border-radius:8px;
    background:linear-gradient(155deg,var(--accent),#8a5cff);
    display:flex;align-items:center;justify-content:center;
    font-family:var(--font-mono);font-weight:600;font-size:13px;color:#fff;
    flex:none;
  }
  .brand-text{display:flex;flex-direction:column;line-height:1.15;}
  .brand-text b{font-family:var(--font-display);font-size:15px;font-weight:600;letter-spacing:.2px;}
  .brand-text span{font-family:var(--font-mono);font-size:11px;color:var(--text-dim);}

  nav.navlist{display:flex;flex-direction:column;gap:2px;margin-top:4px;}
  .navitem{
    display:flex;align-items:center;gap:10px;
    padding:9px 10px;border-radius:var(--radius-sm);
    color:var(--text-muted);font-size:13.5px;font-weight:500;
    border:1px solid transparent;background:none;text-align:left;width:100%;
    transition:background .12s,color .12s;
  }
  .navitem svg{flex:none;width:16px;height:16px;opacity:.85;}
  .navitem:hover{background:var(--surface-2);color:var(--text);}
  .navitem.active{background:var(--accent-soft);color:var(--accent-strong);border-color:rgba(91,140,255,.25);}
  .navitem .count{
    margin-inline-start:auto;font-family:var(--font-mono);font-size:11px;
    color:var(--text-dim);background:var(--surface-2);padding:1px 6px;border-radius:20px;
  }
  .navitem.active .count{color:var(--accent-strong);background:rgba(91,140,255,.16);}

  #sidebar .sidebar-footer{margin-top:auto;padding-top:14px;border-top:1px solid var(--border-soft);}
  .status-pill{
    display:flex;align-items:center;gap:8px;
    padding:9px 10px;border-radius:var(--radius-sm);background:var(--surface-2);
    font-size:12.5px;color:var(--text-muted);
  }
  .status-dot{width:8px;height:8px;border-radius:50%;background:var(--text-dim);flex:none;}
  .status-dot.on{background:var(--success);box-shadow:0 0 0 0 rgba(52,211,153,.5);animation:pulse 2s infinite;}
  .status-dot.off{background:var(--danger);}
  @keyframes pulse{
    0%{box-shadow:0 0 0 0 rgba(52,211,153,.45);}
    70%{box-shadow:0 0 0 6px rgba(52,211,153,0);}
    100%{box-shadow:0 0 0 0 rgba(52,211,153,0);}
  }
  @media (prefers-reduced-motion:reduce){ .status-dot.on{animation:none;} }

  #main{flex:1;min-width:0;padding:28px 34px 60px;max-width:1180px;}
  .page{display:none;}
  .page.active{display:block;animation:fadein .18s ease;}
  @keyframes fadein{from{opacity:0;transform:translateY(3px);}to{opacity:1;transform:none;}}

  .page-head{display:flex;align-items:baseline;justify-content:space-between;gap:16px;margin-bottom:22px;flex-wrap:wrap;}
  .page-head h1{font-family:var(--font-display);font-size:21px;font-weight:600;margin:0;letter-spacing:.1px;}
  .page-head p{margin:4px 0 0;color:var(--text-muted);font-size:13px;max-width:60ch;}

  .panel{
    background:var(--surface);border:1px solid var(--border-soft);
    border-radius:var(--radius);padding:20px 22px;margin-bottom:18px;
  }
  .panel h2{font-family:var(--font-display);font-size:15px;font-weight:600;margin:0 0 4px;}
  .panel .sub{color:var(--text-muted);font-size:12.5px;margin:0 0 16px;}
  .panel-head{display:flex;align-items:center;justify-content:space-between;gap:12px;margin-bottom:14px;}
  .panel-head h2{margin:0;}

  .row{display:flex;gap:12px;flex-wrap:wrap;}
  .field{display:flex;flex-direction:column;gap:6px;flex:1;min-width:150px;}
  .field label{font-size:12px;color:var(--text-muted);font-weight:500;}
  .field .hint{font-size:11px;color:var(--text-dim);}
  input[type=text],input[type=number],input[type=password]{
    background:var(--surface-2);border:1px solid var(--border);color:var(--text);
    border-radius:var(--radius-sm);padding:9px 11px;font-size:13.5px;outline:none;
    transition:border-color .12s,background .12s;
  }
  input:focus{border-color:var(--accent);background:#141a27;}
  input::placeholder{color:var(--text-dim);}

  .btn{
    display:inline-flex;align-items:center;justify-content:center;gap:7px;
    padding:9px 15px;border-radius:var(--radius-sm);border:1px solid var(--border);
    background:var(--surface-2);color:var(--text);font-size:13px;font-weight:600;
    transition:background .12s,border-color .12s,transform .06s;white-space:nowrap;
  }
  .btn:hover{background:var(--surface-hover);}
  .btn:active{transform:scale(.98);}
  .btn:focus-visible{outline:2px solid var(--accent);outline-offset:2px;}
  .btn-primary{background:var(--accent);border-color:var(--accent);color:#fff;}
  .btn-primary:hover{background:var(--accent-strong);}
  .btn-danger{background:transparent;border-color:rgba(240,84,107,.35);color:#ff8a9a;}
  .btn-danger:hover{background:var(--danger-soft);border-color:var(--danger);}
  .btn-ghost{background:transparent;border-color:transparent;color:var(--text-muted);padding:7px 10px;}
  .btn-ghost:hover{background:var(--surface-2);color:var(--text);}
  .btn-sm{padding:6px 10px;font-size:12px;}
  .btn[disabled]{opacity:.5;cursor:not-allowed;}
  .btn svg{width:14px;height:14px;}

  .badge{
    display:inline-flex;align-items:center;gap:5px;font-size:11px;font-weight:600;
    padding:3px 8px;border-radius:20px;font-family:var(--font-mono);letter-spacing:.2px;
  }
  .badge-accent{background:var(--accent-soft);color:var(--accent-strong);}
  .badge-success{background:var(--success-soft);color:var(--success);}
  .badge-muted{background:var(--surface-2);color:var(--text-dim);}

  table.data{width:100%;border-collapse:collapse;font-size:13px;}
  table.data th{
    text-align:left;color:var(--text-dim);font-weight:600;font-size:11px;
    text-transform:uppercase;letter-spacing:.4px;padding:0 10px 8px;border-bottom:1px solid var(--border-soft);
  }
  table.data td{padding:11px 10px;border-bottom:1px solid var(--border-soft);vertical-align:middle;}
  table.data tr:last-child td{border-bottom:none;}
  table.data td.mono{font-family:var(--font-mono);font-size:12.5px;color:var(--text-muted);}
  .empty-row td{color:var(--text-dim);text-align:center;padding:26px 10px;font-size:13px;}

  .warp-group{border:1px solid var(--border-soft);border-radius:var(--radius);margin-bottom:12px;overflow:hidden;}
  .warp-group-head{
    display:flex;align-items:center;gap:12px;padding:14px 16px;cursor:pointer;
    background:var(--surface-2);user-select:none;
  }
  .warp-group-head:hover{background:var(--surface-hover);}
  .warp-group-tag{font-family:var(--font-mono);font-weight:600;font-size:13.5px;}
  .warp-group-meta{color:var(--text-dim);font-size:12px;margin-inline-start:2px;}
  .warp-group-actions{margin-inline-start:auto;display:flex;gap:6px;align-items:center;}
  .warp-group-chevron{width:14px;height:14px;color:var(--text-dim);transition:transform .15s;flex:none;}
  .warp-group.open .warp-group-chevron{transform:rotate(90deg);}
  .warp-group-body{display:none;padding:4px 16px 14px;}
  .warp-group.open .warp-group-body{display:block;}
  .endpoint-row{
    display:flex;align-items:center;gap:10px;padding:8px 4px;
    border-top:1px solid var(--border-soft);font-family:var(--font-mono);font-size:12.5px;color:var(--text-muted);
  }
  .endpoint-row:first-child{border-top:none;}
  .endpoint-host{color:var(--text);}

  .selector-card{
    background:var(--surface-2);border:1px solid var(--border-soft);border-radius:var(--radius-sm);
    padding:12px 14px;margin-bottom:10px;display:flex;align-items:center;gap:14px;flex-wrap:wrap;
  }
  .selector-card strong{font-family:var(--font-mono);font-size:13px;min-width:130px;}
  select.node-select{
    flex:1;min-width:180px;background:var(--surface);border:1px solid var(--border);color:var(--text);
    border-radius:var(--radius-sm);padding:8px 10px;font-size:13px;outline:none;
  }
  select.node-select:focus{border-color:var(--accent);}
  #appsTabs .btn.active{background:var(--accent);color:#fff;border-color:var(--accent);}

  table.data td.live-outbound{min-width:190px;}
  table.data td.live-outbound select.node-select{width:100%;min-width:0;padding:6px 9px;font-size:12.5px;}
  table.data td .hint{font-size:11.5px;color:var(--text-dim);}
  table.data td input[type=text],table.data td input[type=number]{
    padding:6px 8px;font-size:12.5px;min-width:70px;width:100%;
  }

  .cm-shell{border:1px solid var(--border);border-radius:var(--radius-sm);overflow:hidden;}
  .CodeMirror{height:340px;font-family:var(--font-mono) !important;font-size:12.5px;background:#0d1119;}
  .cm-shell .cm-head{
    display:flex;align-items:center;justify-content:space-between;background:var(--surface-2);
    padding:7px 10px;border-bottom:1px solid var(--border);
  }
  .cm-shell .cm-head b{font-size:11.5px;color:var(--text-muted);font-weight:600;letter-spacing:.3px;text-transform:uppercase;}
  .cm-status{font-size:11px;font-family:var(--font-mono);}
  .cm-status.ok{color:var(--success);}
  .cm-status.bad{color:var(--danger);}

  .empty-state{
    text-align:center;padding:44px 20px;color:var(--text-dim);
  }
  .empty-state svg{width:32px;height:32px;margin-bottom:10px;opacity:.6;}
  .empty-state p{margin:0;font-size:13px;}

  #toasts{position:fixed;top:18px;right:18px;z-index:9999;display:flex;flex-direction:column;gap:8px;max-width:340px;}
  .toast{
    background:var(--surface-2);border:1px solid var(--border);border-left:3px solid var(--accent);
    color:var(--text);padding:11px 14px;border-radius:var(--radius-sm);font-size:13px;
    box-shadow:0 8px 24px rgba(0,0,0,.35);animation:toastin .18s ease;
  }
  .toast.success{border-left-color:var(--success);}
  .toast.danger{border-left-color:var(--danger);}
  @keyframes toastin{from{opacity:0;transform:translateX(8px);}to{opacity:1;transform:none;}}

  #loginOverlay{
    position:fixed;inset:0;background:rgba(5,7,12,.82);backdrop-filter:blur(3px);
    display:flex;align-items:center;justify-content:center;z-index:9998;
  }
  #loginOverlay .panel{width:380px;margin:0;}
  #loginOverlay h2{text-align:center;margin-bottom:2px;}
  #loginOverlay .sub{text-align:center;}
  #loginOverlay .field{margin-bottom:12px;}
  #loginError{color:#ff8a9a;font-size:12.5px;margin-top:10px;text-align:center;display:none;}

  #mainContent{display:none;}
  #mainContent.show{display:block;}

  .modal-backdrop{
    position:fixed;inset:0;background:rgba(5,7,12,.75);z-index:9997;
    display:flex;align-items:center;justify-content:center;
  }
  .modal-backdrop.hidden{display:none;}
  .modal{width:420px;max-width:calc(100vw - 40px);}

  @media (max-width:840px){
    #sidebar{position:fixed;left:0;top:0;bottom:0;transform:translateX(-100%);transition:transform .18s;z-index:60;}
    #sidebar.open{transform:none;}
    #main{padding:20px 16px 50px;}
    #mobileTopbar{display:flex;}
  }
  #mobileTopbar{display:none;align-items:center;gap:10px;padding:14px 16px;border-bottom:1px solid var(--border-soft);}
</style>
</head>
<body>

<div id="loginOverlay" style="display:none;">
  <div class="panel">
    <h2>Connect to Clash API</h2>
    <p class="sub">Needed only for live node switching on the Services tab.</p>
    <form id="loginForm" onsubmit="event.preventDefault(); login();">
      <div class="field">
        <label for="controllerInput">External controller address</label>
        <input type="text" id="controllerInput" placeholder="127.0.0.1:9090" value="127.0.0.1:9090" required autocomplete="off">
      </div>
      <div class="field">
        <label for="tokenInput">Secret (optional)</label>
        <input type="password" id="tokenInput" placeholder="Leave empty if no secret is set" autocomplete="new-password">
      </div>
      <button type="submit" class="btn btn-primary" style="width:100%;margin-top:4px;">Connect</button>
      <div id="loginError"></div>
    </form>
  </div>
</div>

<div id="shell">
  <div id="mobileTopbar">
    <button class="btn btn-ghost btn-sm" onclick="toggleSidebar()" aria-label="Menu">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M3 6h18M3 12h18M3 18h18"/></svg>
    </button>
    <b style="font-family:var(--font-display);">sb::manager</b>
  </div>

  <aside id="sidebar">
    <div class="brand">
      <div class="brand-mark">sb</div>
      <div class="brand-text"><b>sb::manager</b><span id="brandSub">sing-box control plane</span></div>
    </div>
    <nav class="navlist" id="navlist">
      <button class="navitem active" data-page="services" onclick="showPage('services')">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="3" y="4" width="18" height="6" rx="1.5"/><rect x="3" y="14" width="18" height="6" rx="1.5"/></svg>
        Services <span class="count" id="countServices">0</span>
      </button>
      <button class="navitem" data-page="warp" onclick="showPage('warp')">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="12" cy="12" r="9"/><path d="M3 12h18M12 3c2.5 2.7 2.5 15.3 0 18M12 3c-2.5 2.7-2.5 15.3 0 18"/></svg>
        WARP Nodes <span class="count" id="countWarp">0</span>
      </button>
      <button class="navitem" data-page="dashboard" onclick="showPage('dashboard')">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="3" y="3" width="7" height="9" rx="1"/><rect x="14" y="3" width="7" height="5" rx="1"/><rect x="14" y="12" width="7" height="9" rx="1"/><rect x="3" y="16" width="7" height="5" rx="1"/></svg>
        Dashboard
      </button>
      <button class="navitem" data-page="apps" onclick="showPage('apps')">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="3" y="3" width="18" height="18" rx="2"/><path d="M3 9h18M9 21V9"/></svg>
        Apps <span class="count" id="countApps">0</span>
      </button>
      <button class="navitem" data-page="raw" onclick="showPage('raw')">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M8 17l-5-5 5-5M16 7l5 5-5 5"/></svg>
        Raw Config
      </button>
      <button class="navitem" data-page="settings" onclick="showPage('settings')">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.7 1.7 0 0 0 .34 1.87l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.7 1.7 0 0 0-1.87-.34 1.7 1.7 0 0 0-1.04 1.56V21a2 2 0 1 1-4 0v-.09A1.7 1.7 0 0 0 9 19.4a1.7 1.7 0 0 0-1.87.34l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06A1.7 1.7 0 0 0 4.6 15a1.7 1.7 0 0 0-1.56-1.04H3a2 2 0 1 1 0-4h.09A1.7 1.7 0 0 0 4.6 9a1.7 1.7 0 0 0-.34-1.87l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06A1.7 1.7 0 0 0 9 4.6a1.7 1.7 0 0 0 1.04-1.56V3a2 2 0 1 1 4 0v.09A1.7 1.7 0 0 0 15 4.6a1.7 1.7 0 0 0 1.87-.34l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06A1.7 1.7 0 0 0 19.4 9a1.7 1.7 0 0 0 1.56 1.04H21a2 2 0 1 1 0 4h-.09A1.7 1.7 0 0 0 19.4 15z"/></svg>
        Settings
      </button>
    </nav>
    <div class="sidebar-footer">
      <div class="status-pill">
        <span class="status-dot" id="statusDot"></span>
        <span id="statusText">Checking...</span>
      </div>
    </div>
  </aside>

  <main id="main">
    <div id="mainContent">

      <!-- ============ SERVICES ============ -->
      <section class="page active" id="page-services">
        <div class="page-head">
          <div>
            <h1>Services</h1>
            <p>Each service is a local mixed inbound routed through its own outbound selector.</p>
          </div>
          <button class="btn btn-ghost btn-sm" onclick="loadAllData()">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M3 12a9 9 0 0 1 15-6.7L21 8M21 3v5h-5M21 12a9 9 0 0 1-15 6.7L3 16M3 21v-5h5"/></svg>
            Refresh
          </button>
        </div>

        <div class="panel" id="statsPanel">
          <div class="panel-head"><h2>Runtime status</h2></div>
          <div class="row" id="statsRow"></div>
        </div>

        <div class="panel">
          <h2>Add a service</h2>
          <p class="sub">Creates a local inbound plus a matching selector, linked to your default WARP group.</p>
          <div class="row" style="align-items:flex-end;">
            <div class="field">
              <label for="svcName">Name</label>
              <input type="text" id="svcName" placeholder="e.g. telegram">
            </div>
            <div class="field">
              <label for="svcPort">Listen port</label>
              <input type="number" id="svcPort" placeholder="2083" min="1" max="65535">
            </div>
            <button class="btn btn-primary" onclick="addService()">
              <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M12 5v14M5 12h14"/></svg>
              Add service
            </button>
          </div>
        </div>

        <div class="panel">
          <div class="panel-head">
            <h2>Active services</h2>
            <p class="sub" style="margin:0;">Live outbound changes take effect immediately via the Clash API, without restarting sing-box.</p>
          </div>
          <table class="data">
            <thead><tr><th>Service</th><th>Inbound</th><th>Port</th><th>Outbound selector</th><th>Live outbound</th><th></th></tr></thead>
            <tbody id="servicesBody"><tr class="empty-row"><td colspan="6">Loading...</td></tr></tbody>
          </table>
        </div>
      </section>

      <!-- ============ WARP NODES ============ -->
      <section class="page" id="page-warp">
        <div class="page-head">
          <div>
            <h1>WARP nodes</h1>
            <p>Grouped by tag. Each group gets its own auto (best-latency) node automatically.</p>
          </div>
        </div>

        <div class="panel">
          <h2>Create a WARP group</h2>
          <p class="sub">Leave the key and reserved fields empty to auto-register a brand-new Cloudflare WARP account.</p>
          <div class="row" style="align-items:flex-end;">
            <div class="field">
              <label for="warpTag">Tag prefix</label>
              <input type="text" id="warpTag" placeholder="WARP-New" value="WARP">
            </div>
            <div class="field" style="flex:2;">
              <label for="warpPriv">Private key <span class="hint">(optional)</span></label>
              <input type="text" id="warpPriv" placeholder="Leave empty to auto-generate">
            </div>
            <div class="field">
              <label for="warpRes">Reserved <span class="hint">(optional)</span></label>
              <input type="text" id="warpRes" placeholder="160,177,129">
            </div>
            <button class="btn btn-primary" onclick="addWarp()">
              <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M12 5v14M5 12h14"/></svg>
              Create group
            </button>
          </div>
        </div>

        <div class="panel">
          <div class="panel-head">
            <h2>Groups</h2>
            <button class="btn btn-ghost btn-sm" onclick="loadWarpGroups()">
              <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M3 12a9 9 0 0 1 15-6.7L21 8M21 3v5h-5M21 12a9 9 0 0 1-15 6.7L3 16M3 21v-5h5"/></svg>
              Refresh
            </button>
          </div>
          <div id="warpGroupsContainer"></div>
        </div>
      </section>

      <!-- ============ RAW CONFIG ============ -->
      <section class="page" id="page-raw">
        <div class="page-head">
          <div>
            <h1>Raw config</h1>
            <p>Direct access to template.json and nodes.json. Changes are validated with sing-box before anything is written.</p>
          </div>
        </div>

        <div class="panel">
          <div class="row" style="align-items:flex-start;">
            <div class="field" style="min-width:340px;">
              <div class="cm-shell">
                <div class="cm-head"><b>template.json</b><span class="cm-status" id="tmplStatus"></span></div>
                <textarea id="tmplEditor"></textarea>
              </div>
            </div>
            <div class="field" style="min-width:340px;">
              <div class="cm-shell">
                <div class="cm-head"><b>nodes.json</b><span class="cm-status" id="nodesStatus"></span></div>
                <textarea id="nodesEditor"></textarea>
              </div>
            </div>
          </div>
          <div class="row" style="margin-top:16px;">
            <button class="btn btn-ghost btn-sm" onclick="formatEditors()">Format JSON</button>
            <button class="btn btn-primary" style="margin-inline-start:auto;" onclick="saveAndRebuild()">
              <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M19 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h11l5 5v11a2 2 0 0 1-2 2z"/><path d="M17 21v-8H7v8M7 3v5h8"/></svg>
              Save &amp; restart sing-box
            </button>
          </div>
        </div>
      </section>

      <!-- ============ SETTINGS ============ -->
      <section class="page" id="page-settings">
        <div class="page-head">
          <div>
            <h1>Settings</h1>
            <p>Manage the management API's admin token and the local sing-box binary.</p>
          </div>
        </div>

        <div class="panel">
          <h2>Admin token</h2>
          <p class="sub">Required as the <code>X-Admin-Token</code> header (or <code>?token=</code> query param) on every <code>/api/*</code> request. Leave empty to disable authentication — only do this on a trusted local network.</p>
          <div id="adminTokenStatus" class="row" style="margin-bottom:14px;"></div>
          <div class="row" style="align-items:flex-end;">
            <div class="field">
              <label for="newAdminToken">New token <span class="hint">(min. 8 characters, or empty to disable)</span></label>
              <input type="password" id="newAdminToken" placeholder="Leave empty to disable authentication" autocomplete="new-password">
            </div>
            <button class="btn btn-primary" onclick="saveAdminToken()">Save</button>
          </div>
        </div>

        <div class="panel">
          <div class="panel-head">
            <h2>sing-box binary</h2>
            <button class="btn btn-ghost btn-sm" onclick="loadSingboxInfo()">
              <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M3 12a9 9 0 0 1 15-6.7L21 8M21 3v5h-5M21 12a9 9 0 0 1-15 6.7L3 16M3 21v-5h5"/></svg>
              Refresh
            </button>
          </div>
          <p class="sub">Detected automatically at startup (PATH, working directory, common install paths) and downloaded automatically if missing.</p>
          <div id="singboxInfo" class="row" style="margin-bottom:14px;"></div>
          <div class="row" style="align-items:flex-end;">
            <div class="field">
              <label for="singboxVersion">Version to install</label>
              <input type="text" id="singboxVersion" placeholder="v1.13.16" value="v1.13.16">
            </div>
            <button class="btn btn-primary" id="singboxDownloadBtn" onclick="downloadSingboxVersion()">
              <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M12 3v13m0 0l-4-4m4 4l4-4M5 21h14"/></svg>
              Download / reinstall
            </button>
          </div>
          <div class="row" style="margin-top:14px;">
            <button class="btn btn-ghost btn-sm" onclick="controlSingbox('start')">Start</button>
            <button class="btn btn-ghost btn-sm" onclick="controlSingbox('restart')">Restart</button>
            <button class="btn btn-danger btn-sm" onclick="controlSingbox('stop')">Stop</button>
          </div>
        </div>

        <div class="panel">
          <div class="panel-head">
            <h2>Cloudflare Tunnel</h2>
            <button class="btn btn-ghost btn-sm" onclick="loadCloudflareSettings()">
              <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M3 12a9 9 0 0 1 15-6.7L21 8M21 3v5h-5M21 12a9 9 0 0 1-15 6.7L3 16M3 21v-5h5"/></svg>
              Refresh
            </button>
          </div>
          <p class="sub">Exposes the admin panel, the Clash dashboard, and every Docker web app below through a public Cloudflare hostname. Proxy services in the Services tab always stay private.</p>
          <div id="cfStatus" class="row" style="margin-bottom:14px;"></div>

          <div class="field">
            <label>Mode</label>
            <select id="cfMode" onchange="updateCloudflareModeUI()">
              <option value="api_token">Full API Token — I own a domain in Cloudflare (fully automatic routes + DNS)</option>
              <option value="tunnel_token">Tunnel Token only — I already created a tunnel and set routes myself</option>
              <option value="quick">None of these — use a free auto-generated *.trycloudflare.com URL</option>
            </select>
          </div>

          <div id="cfFieldsApiToken" class="row" style="align-items:flex-end;">
            <div class="field">
              <label for="cfApiToken">Cloudflare API Token <span class="hint">(Account → Cloudflare Tunnel:Edit, Zone → DNS:Edit)</span></label>
              <input type="password" id="cfApiToken" placeholder="Leave empty to keep the current token" autocomplete="new-password">
            </div>
            <div class="field">
              <label for="cfZoneName">Domain (zone) you own in Cloudflare</label>
              <input type="text" id="cfZoneName" placeholder="example.com">
            </div>
          </div>

          <div id="cfFieldsTunnelToken" class="row" style="align-items:flex-end;">
            <div class="field">
              <label for="cfTunnelToken">Tunnel Token <span class="hint">(from Zero Trust → Networks → Tunnels)</span></label>
              <input type="password" id="cfTunnelToken" placeholder="Leave empty to keep the current token" autocomplete="new-password">
            </div>
            <div class="field">
              <label for="cfDashboardPublicUrl">Clash dashboard public URL <span class="hint">(e.g. https://dash.example.com — the hostname you routed to the Clash API in your own ingress)</span></label>
              <input type="text" id="cfDashboardPublicUrl" placeholder="https://dash.example.com">
            </div>
          </div>

          <div class="row" style="margin-top:12px;">
            <button class="btn btn-primary" onclick="saveCloudflareSettings()">Save</button>
            <button class="btn btn-ghost btn-sm" onclick="controlCloudflare('start')">Start</button>
            <button class="btn btn-ghost btn-sm" onclick="controlCloudflare('restart')">Restart</button>
            <button class="btn btn-danger btn-sm" onclick="controlCloudflare('stop')">Stop</button>
          </div>
          <div id="cfRoutes" style="margin-top:14px;"></div>
        </div>

        <div class="panel">
          <div class="panel-head"><h2>Docker web apps</h2></div>
          <p class="sub">Each entry gets its own tab under "Apps" (shown as an iframe) and, in Full API Token mode, its own public subdomain — e.g. zai/grok/deepseek. <span class="hint">In Tunnel Token mode (manual ingress) or when there's no tunnel at all, set "Public URL" below if the app isn't reachable at this panel's own hostname.</span></p>
          <div class="row" style="align-items:flex-end;">
            <div class="field">
              <label for="dsName">Name <span class="hint">(used as subdomain, lowercase)</span></label>
              <input type="text" id="dsName" placeholder="e.g. zai">
            </div>
            <div class="field">
              <label for="dsPort">Local port</label>
              <input type="number" id="dsPort" placeholder="3000" min="1" max="65535">
            </div>
            <div class="field">
              <label for="dsPublicUrl">Public URL <span class="hint">(optional override)</span></label>
              <input type="text" id="dsPublicUrl" placeholder="https://zai.example.com">
            </div>
            <button class="btn btn-primary" onclick="addDockerService()">Add</button>
          </div>
          <table class="data" style="margin-top:14px;">
            <thead><tr><th>Name</th><th>Local port</th><th>Public URL</th><th></th></tr></thead>
            <tbody id="dockerServicesBody"><tr class="empty-row"><td colspan="4">No Docker web apps added yet.</td></tr></tbody>
          </table>
        </div>

        <div class="panel">
          <div class="panel-head"><h2>Advanced (environment) settings</h2></div>
          <p class="sub">These normally come from environment variables at startup; changing them here overrides that (persisted, and used the next time the relevant action runs). <span class="hint">BIND_ADDR/API_PORT need a manager restart to take effect.</span></p>
          <div id="envSettingsBody"></div>
          <div class="row" style="margin-top:12px;">
            <button class="btn btn-primary" onclick="saveEnvSettings()">Save changes</button>
          </div>
        </div>
      </section>

      <!-- ============ DASHBOARD (metacubexd) ============ -->
      <section class="page" id="page-dashboard">
        <div class="page-head">
          <div>
            <h1>Dashboard</h1>
            <p>Clash dashboard (metacubexd), served locally by sing-box — falls back to the public metacubexd.pages.dev build if the local one isn't ready yet.</p>
          </div>
        </div>
        <div class="panel" style="padding:0;overflow:hidden;">
          <iframe id="dashboardFrame" style="width:100%;height:78vh;border:0;display:block;" referrerpolicy="no-referrer"></iframe>
        </div>
      </section>

      <!-- ============ APPS (Docker web frontends) ============ -->
      <section class="page" id="page-apps">
        <div class="page-head">
          <div>
            <h1>Apps</h1>
            <p>Frontends of Docker web apps configured in Settings (e.g. zai, grok, deepseek).</p>
          </div>
        </div>
        <div id="appsTabs" class="row" style="margin-bottom:12px;"></div>
        <div class="panel" id="appsEmpty" style="display:none;">
          <p class="sub">No Docker web apps configured yet. Add one from the Settings page.</p>
        </div>
        <div class="panel" id="appsFrameWrap" style="padding:0;overflow:hidden;display:none;">
          <iframe id="appsFrame" style="width:100%;height:78vh;border:0;display:block;" referrerpolicy="no-referrer"></iframe>
        </div>
      </section>

    </div>
  </main>
</div>

<div id="toasts"></div>

<div class="modal-backdrop hidden" id="confirmBackdrop">
  <div class="panel modal">
    <h2 id="confirmTitle">Are you sure?</h2>
    <p class="sub" id="confirmBody"></p>
    <div class="row" style="justify-content:flex-end;margin-top:6px;">
      <button class="btn btn-ghost" onclick="closeConfirm()">Cancel</button>
      <button class="btn btn-danger" id="confirmActionBtn">Confirm</button>
    </div>
  </div>
</div>

<script>
(function(){
  'use strict';

  // CodeMirror (~250KB across 6 requests) is only needed on the Raw Config
  // page, so it's lazy-loaded on first visit instead of blocking app startup —
  // this is what made the raw config editor feel slow to load previously.
  var CM_BASE = 'https://cdnjs.cloudflare.com/ajax/libs/codemirror/5.65.16';
  var cmAssetsPromise = null;
  function loadStylesheet(href){
    return new Promise(function(resolve, reject){
      var link = document.createElement('link');
      link.rel = 'stylesheet'; link.href = href;
      link.onload = function(){ resolve(); };
      link.onerror = function(){ reject(new Error('Failed to load ' + href)); };
      document.head.appendChild(link);
    });
  }
  function loadScript(src){
    return new Promise(function(resolve, reject){
      var s = document.createElement('script');
      s.src = src; s.async = true;
      s.onload = function(){ resolve(); };
      s.onerror = function(){ reject(new Error('Failed to load ' + src)); };
      document.head.appendChild(s);
    });
  }
  function loadCodeMirrorAssets(){
    if (!cmAssetsPromise){
      cmAssetsPromise = Promise.all([
        loadStylesheet(CM_BASE + '/codemirror.min.css'),
        loadStylesheet(CM_BASE + '/addon/fold/foldgutter.min.css')
      ]).then(function(){
        return loadScript(CM_BASE + '/codemirror.min.js');
      }).then(function(){
        return Promise.all([
          loadScript(CM_BASE + '/mode/javascript/javascript.min.js'),
          loadScript(CM_BASE + '/addon/edit/matchbrackets.min.js'),
          loadScript(CM_BASE + '/addon/fold/foldcode.min.js'),
          loadScript(CM_BASE + '/addon/fold/foldgutter.min.js'),
          loadScript(CM_BASE + '/addon/fold/brace-fold.min.js')
        ]);
      });
    }
    return cmAssetsPromise;
  }

  // -----------------------------------------------------------------
  // Clash API session (controller address + secret), stored per-tab
  // -----------------------------------------------------------------
  var controllerBase = sessionStorage.getItem('controllerBase') || '';
  var secret = sessionStorage.getItem('secret') || '';

  function getControllerHeaders(){
    return secret ? { 'Authorization': 'Bearer ' + secret } : {};
  }
  function controllerFetch(path, options){
    options = options || {};
    var headers = Object.assign({}, getControllerHeaders(), options.headers || {});
    return fetch(controllerBase + path, Object.assign({}, options, { headers: headers }));
  }

  async function checkStoredCredentials(){
    if (!controllerBase){
      document.getElementById('loginOverlay').style.display = 'flex';
      return;
    }
    try {
      var res = await controllerFetch('/proxies');
      if (res.ok){
        enterApp();
      } else {
        clearCreds();
      }
    } catch (err){
      clearCreds();
    }
  }
  function clearCreds(){
    sessionStorage.removeItem('controllerBase');
    sessionStorage.removeItem('secret');
    controllerBase = '';
    secret = '';
    document.getElementById('loginOverlay').style.display = 'flex';
  }
  function enterApp(){
    document.getElementById('loginOverlay').style.display = 'none';
    document.getElementById('mainContent').classList.add('show');
    loadAllData();
    setInterval(loadSelectors, 12000);
    setInterval(loadStatus, 8000);
  }

  window.login = function(){
    var addr = document.getElementById('controllerInput').value.trim();
    var pass = document.getElementById('tokenInput').value.trim();
    var errEl = document.getElementById('loginError');
    errEl.style.display = 'none';
    if (!addr){
      errEl.textContent = 'Address is required.';
      errEl.style.display = 'block';
      return;
    }
    var base = addr;
    if (base.indexOf('http://') !== 0 && base.indexOf('https://') !== 0) base = 'http://' + base;
    if (base.slice(-1) === '/') base = base.slice(0, -1);

    var headers = pass ? { 'Authorization': 'Bearer ' + pass } : {};
    fetch(base + '/proxies', { headers: headers }).then(function(res){
      if (res.ok){
        controllerBase = base; secret = pass;
        sessionStorage.setItem('controllerBase', base);
        sessionStorage.setItem('secret', pass);
        enterApp();
      } else if (res.status === 401){
        errEl.textContent = 'Invalid secret.';
        errEl.style.display = 'block';
      } else {
        errEl.textContent = 'Unexpected response: ' + res.status;
        errEl.style.display = 'block';
      }
    }).catch(function(){
      errEl.textContent = 'Cannot connect to ' + base;
      errEl.style.display = 'block';
    });
  };

  checkStoredCredentials();

  // -----------------------------------------------------------------
  // Navigation
  // -----------------------------------------------------------------
  window.showPage = function(name){
    document.querySelectorAll('.page').forEach(function(el){ el.classList.remove('active'); });
    document.querySelectorAll('.navitem').forEach(function(el){ el.classList.remove('active'); });
    var page = document.getElementById('page-' + name);
    if (page) page.classList.add('active');
    var nav = document.querySelector('.navitem[data-page="' + name + '"]');
    if (nav) nav.classList.add('active');
    document.getElementById('sidebar').classList.remove('open');
    if (name === 'raw') ensureRawEditors();
    if (name === 'settings'){ loadSettings(); loadSingboxInfo(); loadCloudflareSettings(); loadDockerServices(); loadEnvSettings(); }
    if (name === 'dashboard') loadDashboardFrame();
    if (name === 'apps') loadAppsTabs();
  };
  window.toggleSidebar = function(){
    document.getElementById('sidebar').classList.toggle('open');
  };

  // -----------------------------------------------------------------
  // Toasts
  // -----------------------------------------------------------------
  function showMessage(msg, type){
    var box = document.getElementById('toasts');
    var t = document.createElement('div');
    t.className = 'toast ' + (type === 'danger' ? 'danger' : 'success');
    t.textContent = msg;
    box.appendChild(t);
    setTimeout(function(){ t.remove(); }, 5000);
  }
  window.showMessage = showMessage;

  // -----------------------------------------------------------------
  // Confirm modal (replaces window.confirm for a consistent look)
  // -----------------------------------------------------------------
  function askConfirm(title, body, onConfirm){
    var backdrop = document.getElementById('confirmBackdrop');
    document.getElementById('confirmTitle').textContent = title;
    document.getElementById('confirmBody').textContent = body;
    var btn = document.getElementById('confirmActionBtn');
    var freshBtn = btn.cloneNode(true);
    btn.parentNode.replaceChild(freshBtn, btn);
    freshBtn.addEventListener('click', function(){
      closeConfirm();
      onConfirm();
    });
    backdrop.classList.remove('hidden');
  }
  window.closeConfirm = function(){
    document.getElementById('confirmBackdrop').classList.add('hidden');
  };

  // -----------------------------------------------------------------
  // Generic API helper
  // -----------------------------------------------------------------
  async function api(endpoint, body){
    var res = await fetch(endpoint, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body || {})
    });
    var data = {};
    try { data = await res.json(); } catch (e) {}
    if (!res.ok) throw new Error(data.error || ('Request failed (' + res.status + ')'));
    return data;
  }
  async function request(endpoint, body, successMsg){
    try {
      var data = await api(endpoint, body);
      showMessage(data.message || successMsg || 'Done', 'success');
      loadAllData();
      return true;
    } catch (err){
      showMessage(err.message, 'danger');
      return false;
    }
  }

  // -----------------------------------------------------------------
  // Status
  // -----------------------------------------------------------------
  async function loadStatus(){
    try {
      var res = await fetch('/api/status');
      var data = await res.json();
      var dot = document.getElementById('statusDot');
      var text = document.getElementById('statusText');
      if (data.running){
        dot.className = 'status-dot on';
        text.textContent = 'Running (pid ' + data.pid + ')';
      } else {
        dot.className = 'status-dot off';
        text.textContent = 'Stopped';
      }
      return data;
    } catch (err){
      document.getElementById('statusDot').className = 'status-dot off';
      document.getElementById('statusText').textContent = 'Unreachable';
    }
  }

  // -----------------------------------------------------------------
  // Overview stats
  // -----------------------------------------------------------------
  function statCard(label, value, tone){
    var el = document.createElement('div');
    el.className = 'field';
    el.style.minWidth = '140px';
    var v = document.createElement('div');
    v.style.fontFamily = 'var(--font-display)';
    v.style.fontSize = '22px';
    v.style.fontWeight = '600';
    if (tone) v.style.color = tone;
    v.textContent = value;
    var l = document.createElement('div');
    l.style.fontSize = '12px';
    l.style.color = 'var(--text-muted)';
    l.textContent = label;
    el.appendChild(v); el.appendChild(l);
    return el;
  }
  function renderStats(tmpl, nodesArr, warpData){
    var row = document.getElementById('statsRow');
    row.innerHTML = '';
    var services = (tmpl.inbounds || []).filter(function(i){ return i.tag && i.tag.indexOf('in-') === 0; }).length;
    var groups = (warpData && warpData.groups) ? warpData.groups.length : 0;
    var endpoints = nodesArr.length;
    var defGroup = (warpData && warpData.default_group) ? warpData.default_group : 'none';
    row.appendChild(statCard('Active services', services));
    row.appendChild(statCard('WARP groups', groups));
    row.appendChild(statCard('WARP endpoints', endpoints));
    row.appendChild(statCard('Default group', defGroup, defGroup === 'none' ? 'var(--warning)' : 'var(--success)'));
  }

  // -----------------------------------------------------------------
  // Config editors (CodeMirror) — lazily loaded/initialized on first
  // visit to the Raw Config page instead of on login.
  // -----------------------------------------------------------------
  var tmplCM, nodesCM, editorsReady = false, editorsLoading = false;
  function ensureRawEditors(){
    if (editorsReady || editorsLoading) return;
    editorsLoading = true;
    var tmplStatusEl = document.getElementById('tmplStatus');
    var nodesStatusEl = document.getElementById('nodesStatus');
    tmplStatusEl.textContent = 'loading editor…';
    nodesStatusEl.textContent = 'loading editor…';
    loadCodeMirrorAssets().then(function(){
      initEditors();
      editorsLoading = false;
      validateEditor(tmplCM, 'tmplStatus');
      validateEditor(nodesCM, 'nodesStatus');
    }).catch(function(err){
      editorsLoading = false;
      tmplStatusEl.textContent = '';
      nodesStatusEl.textContent = '';
      showMessage('Failed to load the code editor: ' + err.message, 'danger');
    });
  }
  function initEditors(){
    if (editorsReady) return;
    editorsReady = true;
    var opts = {
      mode: { name: 'javascript', json: true },
      lineNumbers: true,
      matchBrackets: true,
      foldGutter: true,
      gutters: ['CodeMirror-linenumbers', 'CodeMirror-foldgutter'],
      tabSize: 2,
      theme: 'default'
    };
    tmplCM = CodeMirror.fromTextArea(document.getElementById('tmplEditor'), opts);
    nodesCM = CodeMirror.fromTextArea(document.getElementById('nodesEditor'), opts);
    tmplCM.on('change', function(){ validateEditor(tmplCM, 'tmplStatus'); });
    nodesCM.on('change', function(){ validateEditor(nodesCM, 'nodesStatus'); });
  }
  function validateEditor(cm, statusId){
    var el = document.getElementById(statusId);
    try {
      JSON.parse(cm.getValue());
      el.textContent = 'valid';
      el.className = 'cm-status ok';
    } catch (e){
      el.textContent = 'invalid JSON';
      el.className = 'cm-status bad';
    }
  }
  window.formatEditors = function(){
    if (!editorsReady){ showMessage('Editor is still loading…', 'danger'); return; }
    [ [tmplCM, 'tmplStatus'], [nodesCM, 'nodesStatus'] ].forEach(function(pair){
      var cm = pair[0];
      try {
        var parsed = JSON.parse(cm.getValue());
        cm.setValue(JSON.stringify(parsed, null, 2));
      } catch (e){
        showMessage('Cannot format ' + pair[1].replace('Status','') + ': invalid JSON', 'danger');
      }
    });
  };

  // -----------------------------------------------------------------
  // Data loading
  // -----------------------------------------------------------------
  var lastTemplate = {}, lastNodes = [];

  window.loadAllData = function(){
    loadStatus();
    loadConfigsAndDerived();
    loadSelectors();
  };

  async function loadConfigsAndDerived(){
    try {
      var res = await fetch('/api/get_configs');
      var data = await res.json();
      // Always keep the plain textareas in sync so CodeMirror picks up the
      // right content whenever it's lazily initialized on the Raw Config page.
      document.getElementById('tmplEditor').value = data.template || '{}';
      document.getElementById('nodesEditor').value = data.nodes || '[]';
      if (editorsReady){
        tmplCM.setValue(data.template || '{}');
        nodesCM.setValue(data.nodes || '[]');
        validateEditor(tmplCM, 'tmplStatus');
        validateEditor(nodesCM, 'nodesStatus');
      }
      try { lastTemplate = JSON.parse(data.template || '{}'); } catch(e){ lastTemplate = {}; }
      try { lastNodes = JSON.parse(data.nodes || '[]'); } catch(e){ lastNodes = []; }
      renderServices(lastTemplate);
      await loadWarpGroups();
      loadSelectors();
    } catch (err){
      showMessage('Failed to load configuration: ' + err.message, 'danger');
    }
  }

  function renderServices(tmpl){
    var body = document.getElementById('servicesBody');
    var inbounds = (tmpl.inbounds || []).filter(function(i){ return i.tag && i.tag.indexOf('in-') === 0; });
    document.getElementById('countServices').textContent = inbounds.length;
    if (inbounds.length === 0){
      body.innerHTML = '<tr class="empty-row"><td colspan="6">No services yet — add one above.</td></tr>';
      return;
    }
    body.innerHTML = '';
    inbounds.forEach(function(inb){
      var name = inb.tag.substring(3);
      var port = inb.listen_port;
      var tr = document.createElement('tr');
      tr.dataset.service = name;

      var tdName = document.createElement('td'); tdName.textContent = name;
      var tdTag = document.createElement('td'); tdTag.className = 'mono'; tdTag.textContent = inb.tag;
      var tdPort = document.createElement('td'); tdPort.className = 'mono'; tdPort.textContent = port;
      var tdSel = document.createElement('td'); tdSel.className = 'mono'; tdSel.textContent = 'select-' + name;

      // Populated/refreshed by loadSelectors() once the Clash API is reachable.
      var tdLive = document.createElement('td');
      tdLive.className = 'live-outbound';
      tdLive.dataset.service = name;
      tdLive.innerHTML = '<span class="hint">' + (controllerBase ? 'loading…' : 'connect to switch live') + '</span>';

      var tdAct = document.createElement('td');
      tdAct.style.whiteSpace = 'nowrap';

      var editBtn = document.createElement('button');
      editBtn.className = 'btn btn-ghost btn-sm';
      editBtn.textContent = 'Edit';
      editBtn.onclick = function(){ startEditService(tr, name, port); };

      var delBtn = document.createElement('button');
      delBtn.className = 'btn btn-danger btn-sm';
      delBtn.textContent = 'Delete';
      delBtn.style.marginInlineStart = '6px';
      delBtn.onclick = function(){
        askConfirm('Delete service', 'This removes the "' + name + '" inbound, its selector, and its routing rule.', function(){
          request('/api/delete_service', { name: name }, 'Service deleted');
        });
      };
      tdAct.appendChild(editBtn);
      tdAct.appendChild(delBtn);

      tr.appendChild(tdName); tr.appendChild(tdTag); tr.appendChild(tdPort); tr.appendChild(tdSel); tr.appendChild(tdLive); tr.appendChild(tdAct);
      body.appendChild(tr);
    });
  }

  // Swaps the Name/Port cells for inputs and the action buttons for Save/Cancel,
  // without disturbing the rest of the table.
  function startEditService(tr, name, port){
    var tdName = tr.children[0];
    var tdPort = tr.children[2];
    var tdAct = tr.children[5];

    tdName.innerHTML = '';
    var nameInput = document.createElement('input');
    nameInput.type = 'text';
    nameInput.value = name;
    nameInput.maxLength = 32;
    tdName.appendChild(nameInput);

    tdPort.innerHTML = '';
    var portInput = document.createElement('input');
    portInput.type = 'number';
    portInput.value = port;
    portInput.min = 1;
    portInput.max = 65535;
    tdPort.appendChild(portInput);

    tdAct.innerHTML = '';
    var saveBtn = document.createElement('button');
    saveBtn.className = 'btn btn-primary btn-sm';
    saveBtn.textContent = 'Save';
    saveBtn.onclick = function(){
      var newName = nameInput.value.trim();
      var newPort = parseInt(portInput.value, 10);
      if (!newName){ showMessage('Service name is required', 'danger'); return; }
      if (isNaN(newPort) || newPort < 1 || newPort > 65535){ showMessage('A valid port (1-65535) is required', 'danger'); return; }
      saveBtn.disabled = true;
      request('/api/edit_service', { old_name: name, new_name: newName, new_port: newPort }, 'Service updated').then(function(ok){
        if (!ok) saveBtn.disabled = false;
      });
    };

    var cancelBtn = document.createElement('button');
    cancelBtn.className = 'btn btn-ghost btn-sm';
    cancelBtn.textContent = 'Cancel';
    cancelBtn.style.marginInlineStart = '6px';
    cancelBtn.onclick = function(){ renderServices(lastTemplate); loadSelectors(); };

    tdAct.appendChild(saveBtn);
    tdAct.appendChild(cancelBtn);

    nameInput.focus();
    nameInput.select();
  }

  window.addService = function(){
    var name = document.getElementById('svcName').value.trim();
    var port = parseInt(document.getElementById('svcPort').value, 10);
    if (!name){ showMessage('Service name is required', 'danger'); return; }
    if (isNaN(port) || port < 1 || port > 65535){ showMessage('A valid port (1-65535) is required', 'danger'); return; }
    request('/api/add_service', { name: name, port: port }, 'Service added').then(function(ok){
      if (ok){
        document.getElementById('svcName').value = '';
        document.getElementById('svcPort').value = '';
      }
    });
  };

  window.saveAndRebuild = function(){
    if (!editorsReady){ showMessage('Editors are not ready yet', 'danger'); return; }
    var tmpl = tmplCM.getValue();
    var nodes = nodesCM.getValue();
    try { JSON.parse(tmpl); JSON.parse(nodes); } catch (e){
      showMessage('Fix the JSON errors before saving', 'danger'); return;
    }
    request('/api/rebuild', { template: tmpl, nodes: nodes }, 'Configuration saved and sing-box restarted');
  };

  // -----------------------------------------------------------------
  // WARP groups
  // -----------------------------------------------------------------
  var lastWarpData = { groups: [], default_group: '' };

  async function loadWarpGroups(){
    try {
      var res = await fetch('/api/warp_groups');
      var data = await res.json();
      lastWarpData = data;
      document.getElementById('countWarp').textContent = (data.groups || []).length;
      renderWarpGroups(data);
      renderStats(lastTemplate, lastNodes, data);
      return data;
    } catch (err){
      showMessage('Failed to load WARP groups: ' + err.message, 'danger');
    }
  }
  window.loadWarpGroups = loadWarpGroups;

  function renderWarpGroups(data){
    var container = document.getElementById('warpGroupsContainer');
    var groups = data.groups || [];
    if (groups.length === 0){
      container.innerHTML = '<div class="empty-state"><p>No WARP groups yet. Create one above to get started.</p></div>';
      return;
    }
    container.innerHTML = '';
    groups.forEach(function(g){
      var card = document.createElement('div');
      card.className = 'warp-group';

      var head = document.createElement('div');
      head.className = 'warp-group-head';
      head.onclick = function(){ card.classList.toggle('open'); };

      var chevron = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
      chevron.setAttribute('viewBox', '0 0 24 24');
      chevron.setAttribute('fill', 'none');
      chevron.setAttribute('stroke', 'currentColor');
      chevron.setAttribute('stroke-width', '2');
      chevron.setAttribute('class', 'warp-group-chevron');
      chevron.innerHTML = '<path d="M9 6l6 6-6 6"/>';

      var tag = document.createElement('span');
      tag.className = 'warp-group-tag';
      tag.textContent = g.tag;

      var meta = document.createElement('span');
      meta.className = 'warp-group-meta';
      meta.textContent = g.count + ' endpoint' + (g.count === 1 ? '' : 's') + ' - auto tag ' + g.auto_tag;

      head.appendChild(chevron);
      head.appendChild(tag);
      if (g.is_default){
        var badge = document.createElement('span');
        badge.className = 'badge badge-success';
        badge.textContent = 'Default';
        head.appendChild(badge);
      }
      head.appendChild(meta);

      var actions = document.createElement('div');
      actions.className = 'warp-group-actions';

      if (!g.is_default){
        var defBtn = document.createElement('button');
        defBtn.className = 'btn btn-ghost btn-sm';
        defBtn.textContent = 'Set default';
        defBtn.onclick = function(ev){
          ev.stopPropagation();
          request('/api/set_default_warp_group', { tag: g.tag }, 'Default WARP group updated');
        };
        actions.appendChild(defBtn);
      }

      var renameBtn = document.createElement('button');
      renameBtn.className = 'btn btn-ghost btn-sm';
      renameBtn.textContent = 'Rename';
      renameBtn.onclick = function(ev){
        ev.stopPropagation();
        var next = prompt('New tag prefix for "' + g.tag + '":', g.tag);
        if (next === null) return;
        next = next.trim();
        if (!next || next === g.tag) return;
        request('/api/edit_warp_group', { old_tag: g.tag, new_tag: next }, 'Group renamed');
      };
      actions.appendChild(renameBtn);

      var delBtn = document.createElement('button');
      delBtn.className = 'btn btn-danger btn-sm';
      delBtn.textContent = 'Delete';
      delBtn.onclick = function(ev){
        ev.stopPropagation();
        askConfirm('Delete WARP group', 'This removes all ' + g.count + ' endpoint(s) under "' + g.tag + '". Any selector still pointing to it will fall back automatically.', function(){
          request('/api/delete_warp_group', { tag: g.tag }, 'Group deleted');
        });
      };
      actions.appendChild(delBtn);
      head.appendChild(actions);

      var body = document.createElement('div');
      body.className = 'warp-group-body';
      (g.endpoints || []).forEach(function(ep){
        var row = document.createElement('div');
        row.className = 'endpoint-row';
        var host = document.createElement('span');
        host.className = 'endpoint-host';
        host.textContent = ep.host + ':' + ep.port;
        var tagSpan = document.createElement('span');
        tagSpan.style.color = 'var(--text-dim)';
        tagSpan.style.marginInlineStart = 'auto';
        tagSpan.style.marginInlineEnd = '10px';
        tagSpan.textContent = ep.tag;
        var rmBtn = document.createElement('button');
        rmBtn.className = 'btn btn-ghost btn-sm';
        rmBtn.textContent = 'Remove';
        rmBtn.onclick = function(){
          askConfirm('Remove endpoint', 'Remove ' + ep.tag + ' from this group?', function(){
            request('/api/delete_warp_node', { tag: ep.tag }, 'Endpoint removed');
          });
        };
        row.appendChild(host); row.appendChild(tagSpan); row.appendChild(rmBtn);
        body.appendChild(row);
      });

      card.appendChild(head);
      card.appendChild(body);
      container.appendChild(card);
    });
  }

  window.addWarp = function(){
    var tag = document.getElementById('warpTag').value.trim() || 'WARP';
    var priv = document.getElementById('warpPriv').value.trim();
    var resStr = document.getElementById('warpRes').value.trim();
    var reserved = [];
    if (resStr){
      reserved = resStr.split(',').map(function(s){ return parseInt(s.trim(), 10); }).filter(function(n){ return !isNaN(n); });
    }
    request('/api/add_warp', { tag: tag, private_key: priv, reserved: reserved }, 'WARP group created').then(function(ok){
      if (ok) document.getElementById('warpPriv').value = '';
    });
  };

  // -----------------------------------------------------------------
  // Live selectors (Clash API) — populates the "Live outbound" column
  // in the Services table (one cell per service row).
  // -----------------------------------------------------------------
  async function loadSelectors(){
    var cells = document.querySelectorAll('td.live-outbound');
    if (!cells.length) return;
    if (!controllerBase){
      cells.forEach(function(td){ td.innerHTML = '<span class="hint">connect to switch live</span>'; });
      return;
    }
    try {
      var res = await controllerFetch('/proxies');
      if (!res.ok) throw new Error('API responded with status ' + res.status);
      var data = await res.json();
      cells.forEach(function(td){
        var name = td.dataset.service;
        var proxyName = 'select-' + name;
        var proxy = data.proxies ? data.proxies[proxyName] : null;
        if (!proxy || !proxy.type || proxy.type.toLowerCase() !== 'selector'){
          td.innerHTML = '<span class="hint">not available</span>';
          return;
        }
        var select = document.createElement('select');
        select.className = 'node-select';
        if (proxy.all && proxy.all.length){
          proxy.all.forEach(function(nodeName){
            var opt = document.createElement('option');
            opt.value = nodeName; opt.textContent = nodeName;
            if (nodeName === proxy.now) opt.selected = true;
            select.appendChild(opt);
          });
        } else {
          var opt2 = document.createElement('option');
          opt2.textContent = 'No nodes available';
          select.appendChild(opt2);
        }
        select.onchange = async function(e){
          try {
            var putRes = await controllerFetch('/proxies/' + proxyName, {
              method: 'PUT',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({ name: e.target.value })
            });
            if (!putRes.ok) throw new Error('Update failed');
            select.style.borderColor = 'var(--success)';
            setTimeout(function(){ select.style.borderColor = ''; }, 1000);
          } catch (err){
            showMessage('Failed to switch node: ' + err.message, 'danger');
          }
        };
        td.innerHTML = '';
        td.appendChild(select);
      });
    } catch (err){
      cells.forEach(function(td){ td.innerHTML = '<span class="hint">Clash API unreachable</span>'; });
    }
  }
  window.loadSelectors = loadSelectors;

  // -----------------------------------------------------------------
  // Settings: admin token + sing-box binary
  // -----------------------------------------------------------------
  async function loadSettings(){
    try {
      var res = await fetch('/api/settings');
      var data = await res.json();
      var box = document.getElementById('adminTokenStatus');
      box.innerHTML = '';
      box.appendChild(statCard('Status', data.admin_token_set ? 'Protected' : 'Not set', data.admin_token_set ? 'var(--success)' : 'var(--danger)'));
    } catch (err){
      showMessage('Failed to load settings: ' + err.message, 'danger');
    }
  }
  window.saveAdminToken = function(){
    var input = document.getElementById('newAdminToken');
    var token = input.value;
    var trimmed = token.trim();
    var doSave = function(){
      api('/api/settings/admin_token', { new_token: token }).then(function(data){
        showMessage(data.message || 'Admin token updated', 'success');
        input.value = '';
        loadSettings();
      }).catch(function(err){
        showMessage(err.message, 'danger');
      });
    };
    if (!trimmed){
      askConfirm('Disable authentication?', 'The management API will accept requests from anyone who can reach it. Only do this on a trusted local network.', doSave);
    } else if (trimmed.length < 8){
      showMessage('Token must be at least 8 characters (or empty to disable)', 'danger');
    } else {
      doSave();
    }
  };

  async function loadSingboxInfo(){
    try {
      var res = await fetch('/api/singbox/info');
      var data = await res.json();
      var box = document.getElementById('singboxInfo');
      box.innerHTML = '';
      box.appendChild(statCard('Detected', data.found ? 'Yes' : 'No', data.found ? 'var(--success)' : 'var(--danger)'));
      box.appendChild(statCard('Version', data.version || '—'));
      box.appendChild(statCard('Platform', data.os + '/' + data.arch));
      if (data.path) box.appendChild(statCard('Path', data.path));
      var versionInput = document.getElementById('singboxVersion');
      if (versionInput && !versionInput.value) versionInput.value = data.default_version || 'v1.13.16';
    } catch (err){
      showMessage('Failed to load sing-box info: ' + err.message, 'danger');
    }
  }
  window.downloadSingboxVersion = function(){
    var version = document.getElementById('singboxVersion').value.trim() || 'v1.13.16';
    var btn = document.getElementById('singboxDownloadBtn');
    btn.disabled = true;
    showMessage('Downloading sing-box ' + version + ' — this can take a moment…', 'success');
    api('/api/singbox/download', { version: version }).then(function(data){
      showMessage(data.message || 'sing-box downloaded', 'success');
      loadSingboxInfo();
      loadStatus();
    }).catch(function(err){
      showMessage('Download failed: ' + err.message, 'danger');
    }).finally(function(){
      btn.disabled = false;
    });
  };

  window.controlSingbox = function(action){
    api('/api/singbox/' + action, {}).then(function(data){
      showMessage(data.message || ('sing-box ' + action), 'success');
      loadStatus();
    }).catch(function(err){
      showMessage(err.message, 'danger');
    });
  };

  // -----------------------------------------------------------------
  // Cloudflare Tunnel
  // -----------------------------------------------------------------
  window.updateCloudflareModeUI = function(){
    var mode = document.getElementById('cfMode').value;
    document.getElementById('cfFieldsApiToken').style.display = (mode === 'api_token') ? 'flex' : 'none';
    document.getElementById('cfFieldsTunnelToken').style.display = (mode === 'tunnel_token') ? 'flex' : 'none';
  };

  async function loadCloudflareSettings(){
    try {
      var res = await fetch('/api/cloudflare/settings');
      var data = await res.json();
      var box = document.getElementById('cfStatus');
      box.innerHTML = '';
      box.appendChild(statCard('Mode', data.mode || 'not configured'));
      box.appendChild(statCard('Running', data.running ? 'Yes' : 'No', data.running ? 'var(--success)' : 'var(--danger)'));
      if (data.mode) document.getElementById('cfMode').value = data.mode;
      updateCloudflareModeUI();
      if (data.zone_name) document.getElementById('cfZoneName').value = data.zone_name;
      document.getElementById('cfDashboardPublicUrl').value = data.dashboard_public_url || '';
      cfInfoPromise = Promise.resolve(data); // همین پاسخ را برای resolveServicePublicUrl/loadDashboardFrame هم کش کن

      var routesBox = document.getElementById('cfRoutes');
      routesBox.innerHTML = '';
      var urls = [];
      if (data.routes && data.routes.length){
        urls = data.routes.map(function(h){ return 'https://' + h; });
      } else if (data.quick_tunnel_urls){
        urls = Object.keys(data.quick_tunnel_urls).map(function(k){ return k + ': ' + data.quick_tunnel_urls[k]; });
      }
      if (urls.length){
        var list = document.createElement('div');
        list.className = 'hint';
        list.innerHTML = 'Public URLs:<br>' + urls.map(function(u){ return '<span class="mono">' + u + '</span>'; }).join('<br>');
        routesBox.appendChild(list);
      }
    } catch (err){
      showMessage('Failed to load Cloudflare settings: ' + err.message, 'danger');
    }
  }
  window.loadCloudflareSettings = loadCloudflareSettings;

  window.saveCloudflareSettings = function(){
    var body = {
      mode: document.getElementById('cfMode').value,
      api_token: document.getElementById('cfApiToken').value,
      zone_name: document.getElementById('cfZoneName').value.trim(),
      tunnel_token: document.getElementById('cfTunnelToken').value,
      dashboard_public_url: document.getElementById('cfDashboardPublicUrl').value.trim()
    };
    api('/api/cloudflare/settings', body).then(function(data){
      showMessage(data.message || 'Saved', 'success');
      document.getElementById('cfApiToken').value = '';
      document.getElementById('cfTunnelToken').value = '';
      cfInfoPromise = null; // مد/دامنه/آدرس داشبورد ممکن است عوض شده باشد
      loadCloudflareSettings();
    }).catch(function(err){
      showMessage(err.message, 'danger');
    });
  };

  window.controlCloudflare = function(action){
    showMessage('Cloudflare tunnel: ' + action + '…', 'success');
    api('/api/cloudflare/' + action, {}).then(function(data){
      showMessage(data.message || action, 'success');
      cfInfoPromise = null;
      loadCloudflareSettings();
    }).catch(function(err){
      showMessage(err.message, 'danger');
    });
  };

  // -----------------------------------------------------------------
  // resolveServicePublicUrl/loadDashboardFrame یک نمونه از پاسخ
  // /api/cloudflare/settings را کش می‌کنند تا هر بار که تب Apps/Dashboard باز
  // می‌شود دوباره fetch نزنند؛ هر جا mode/دامنه/سرویس‌ها ممکن است عوض شده باشد
  // (ذخیره‌ی تنظیمات Cloudflare، افزودن/حذف Docker service) کش را invalidate می‌کنیم.
  // -----------------------------------------------------------------
  var cfInfoPromise = null;
  function getCloudflareInfo(){
    if (!cfInfoPromise){
      cfInfoPromise = fetch('/api/cloudflare/settings').then(function(r){ return r.json(); }).catch(function(){ return {}; });
    }
    return cfInfoPromise;
  }

  // parseUrlParts یک URL کامل (مثلاً از dashboard_public_url یا quick_tunnel_urls) را
  // به scheme/host/port می‌شکند تا بشود دوباره برایش query string ساخت.
  function parseUrlParts(url){
    try {
      var u = new URL(url);
      var scheme = u.protocol.replace(':', '');
      return { scheme: scheme, host: u.hostname, port: u.port || (scheme === 'https' ? '443' : '80') };
    } catch (e){
      return { scheme: 'https', host: url, port: '443' };
    }
  }

  // -----------------------------------------------------------------
  // Docker web apps (zai/grok/deepseek/...): Settings CRUD + Apps tabs
  // -----------------------------------------------------------------
  async function loadDockerServices(){
    try {
      var res = await fetch('/api/docker_services');
      var data = await res.json();
      var body = document.getElementById('dockerServicesBody');
      if (!body) return data.services || [];
      var services = data.services || [];
      if (!services.length){
        body.innerHTML = '<tr class="empty-row"><td colspan="4">No Docker web apps added yet.</td></tr>';
        return services;
      }
      body.innerHTML = '';
      services.forEach(function(s){
        var tr = document.createElement('tr');
        var tdName = document.createElement('td'); tdName.textContent = s.name;
        var tdPort = document.createElement('td'); tdPort.className = 'mono'; tdPort.textContent = s.port;
        var tdUrl = document.createElement('td'); tdUrl.className = 'mono'; tdUrl.textContent = s.public_url || 'auto';
        var tdAct = document.createElement('td');
        var delBtn = document.createElement('button');
        delBtn.className = 'btn btn-danger btn-sm';
        delBtn.textContent = 'Delete';
        delBtn.onclick = function(){
          askConfirm('Remove ' + s.name + '?', 'This removes its Apps tab and (if Full API Token mode is on) its public route.', function(){
            api('/api/delete_docker_service', { name: s.name }).then(function(data){
              showMessage(data.message || 'Removed', 'success');
              cfInfoPromise = null; // ست سرویس‌ها عوض شد؛ نگاشت هاست‌نیم‌ها باید دوباره خوانده شود
              loadDockerServices();
            }).catch(function(err){ showMessage(err.message, 'danger'); });
          });
        };
        tdAct.appendChild(delBtn);
        tr.appendChild(tdName); tr.appendChild(tdPort); tr.appendChild(tdUrl); tr.appendChild(tdAct);
        body.appendChild(tr);
      });
      return services;
    } catch (err){
      showMessage('Failed to load Docker web apps: ' + err.message, 'danger');
      return [];
    }
  }
  window.loadDockerServices = loadDockerServices;

  window.addDockerService = function(){
    var name = document.getElementById('dsName').value.trim().toLowerCase();
    var port = parseInt(document.getElementById('dsPort').value, 10);
    var publicUrl = document.getElementById('dsPublicUrl').value.trim();
    if (!name){ showMessage('Name is required', 'danger'); return; }
    if (isNaN(port) || port < 1 || port > 65535){ showMessage('A valid port (1-65535) is required', 'danger'); return; }
    api('/api/add_docker_service', { name: name, port: port, public_url: publicUrl }).then(function(data){
      showMessage(data.message || 'Added', 'success');
      document.getElementById('dsName').value = '';
      document.getElementById('dsPort').value = '';
      document.getElementById('dsPublicUrl').value = '';
      cfInfoPromise = null; // ست سرویس‌ها عوض شد؛ نگاشت هاست‌نیم‌ها باید دوباره خوانده شود
      loadDockerServices();
    }).catch(function(err){
      showMessage(err.message, 'danger');
    });
  };

  var appsFrameTimeout = null;
  async function loadAppsTabs(){
    var services = await loadDockerServices();
    document.getElementById('countApps').textContent = services.length;
    var tabsBox = document.getElementById('appsTabs');
    var emptyBox = document.getElementById('appsEmpty');
    var frameWrap = document.getElementById('appsFrameWrap');
    tabsBox.innerHTML = '';
    if (!services.length){
      emptyBox.style.display = '';
      frameWrap.style.display = 'none';
      return;
    }
    emptyBox.style.display = 'none';
    frameWrap.style.display = '';
    services.forEach(function(s, idx){
      var btn = document.createElement('button');
      btn.className = 'btn btn-ghost btn-sm' + (idx === 0 ? ' active' : '');
      btn.textContent = s.name;
      btn.onclick = function(){
        document.querySelectorAll('#appsTabs .btn').forEach(function(b){ b.classList.remove('active'); });
        btn.classList.add('active');
        showAppFrame(s);
      };
      tabsBox.appendChild(btn);
    });
    showAppFrame(services[0]);
  }
  window.loadAppsTabs = loadAppsTabs;

  // resolveServicePublicUrl آدرس واقعیِ قابل‌دسترسی این Docker service را برمی‌گرداند.
  // ترتیب اولویت:
  //   ۱) override دستی service.public_url (کاربر خودش در Settings ست کرده)
  //   ۲) هاست‌نیم واقعی‌ای که تونل Cloudflare در حالت api_token برای این سرویس ساخته
  //      (بدون پورت، چون تونل روی 443 با SNI/هاست‌نیم روت می‌کند، نه با پورت محلی)
  //   ۳) حالت quick: Docker serviceها اصلاً تونل نمی‌شوند → null (پیام مناسب نشان داده می‌شود)
  //   ۴) در غیر این صورت (بدون تونل، دسترسی مستقیم با IP/دامنه‌ی متصل به IP): همان
  //      رفتار قدیمی hostname:port که فقط برای این حالت واقعاً درست است
  async function resolveServicePublicUrl(service){
    if (service.public_url){
      return service.public_url.replace(/\/+$/, '') + (service.path || '/');
    }
    var cf = await getCloudflareInfo();
    if (cf.mode === 'api_token' && cf.service_hosts && cf.service_hosts[service.name]){
      return 'https://' + cf.service_hosts[service.name] + (service.path || '/');
    }
    if (cf.mode === 'quick'){
      return null;
    }
    return 'http://' + window.location.hostname + ':' + service.port + (service.path || '/');
  }

  async function showAppFrame(service){
    var frame = document.getElementById('appsFrame');
    var url = await resolveServicePublicUrl(service);
    if (!url){
      frame.removeAttribute('src');
      showMessage('"' + service.name + '" has no public route under a Quick Tunnel. Switch to Full API Token mode, or set a Public URL for it in Settings.', 'danger');
      return;
    }
    frame.src = url;
  }

  // -----------------------------------------------------------------
  // Dashboard (metacubexd): self-hosted via sing-box external_ui first,
  // falls back to the public metacubexd.pages.dev build if unreachable.
  // -----------------------------------------------------------------
  // -----------------------------------------------------------------
  // Advanced (env-derived) settings
  // -----------------------------------------------------------------
  var envSettingLabels = {
    BIND_ADDR: 'Bind address (needs manager restart)',
    API_PORT: 'API port (needs manager restart)',
    SINGBOX_PATH: 'sing-box binary path override',
    SINGBOX_VERSION: 'sing-box default version',
    SINGBOX_INSTALL_DIR: 'sing-box install directory',
    SINGBOX_NO_AUTO_DOWNLOAD: 'Disable sing-box auto-download (set to "1")',
    CLOUDFLARED_PATH: 'cloudflared binary path override',
    CLOUDFLARED_INSTALL_DIR: 'cloudflared install directory'
  };
  async function loadEnvSettings(){
    try {
      var res = await fetch('/api/settings/env');
      var data = await res.json();
      var box = document.getElementById('envSettingsBody');
      box.innerHTML = '';
      Object.keys(envSettingLabels).forEach(function(key){
        var s = (data.settings && data.settings[key]) || {};
        var row = document.createElement('div');
        row.className = 'field';
        row.style.marginBottom = '10px';
        var label = document.createElement('label');
        label.textContent = envSettingLabels[key];
        var input = document.createElement('input');
        input.type = 'text';
        input.id = 'env_' + key;
        input.value = s.value || '';
        input.placeholder = s.env_value_present ? '(set via environment variable)' : '';
        row.appendChild(label);
        row.appendChild(input);
        box.appendChild(row);
      });
    } catch (err){
      showMessage('Failed to load advanced settings: ' + err.message, 'danger');
    }
  }
  window.loadEnvSettings = loadEnvSettings;

  window.saveEnvSettings = function(){
    var body = {};
    Object.keys(envSettingLabels).forEach(function(key){
      var input = document.getElementById('env_' + key);
      if (input) body[key] = input.value;
    });
    api('/api/settings/env/update', body).then(function(data){
      showMessage(data.message || 'Saved', 'success');
      loadEnvSettings();
    }).catch(function(err){
      showMessage(err.message, 'danger');
    });
  };

  async function loadDashboardFrame(){
    var frame = document.getElementById('dashboardFrame');
    var localPort = '9090', secret = '';
    if (lastTemplate){
      try {
        var clash = lastTemplate.experimental.clash_api;
        if (clash.external_controller) localPort = clash.external_controller.split(':').pop();
        if (clash.secret) secret = clash.secret;
      } catch (e){ /* template not loaded yet, use defaults */ }
    }

    // مثل resolveServicePublicUrl: به‌جای فرض "همین هاست، پورت دیگر"، آدرس واقعی
    // Clash API را از یکی از این منابع می‌گیریم (به ترتیب اولویت):
    //   ۱) dashboard_public_url دستی (حالت tunnel_token با ingress دستی)
    //   ۲) هاست‌نیم "dash.<domain>" که تونل api_token خودکار ساخته (پورت 443، https)
    //   ۳) URL کامل Quick Tunnel برای dash (هم پورت هم اسکیم را از خودش می‌گیریم)
    //   ۴) بدون تونل: همان رفتار قدیمی hostname:پورت محلی
    var cf = await getCloudflareInfo();
    var scheme, host, port;
    if (cf.dashboard_public_url){
      var p = parseUrlParts(cf.dashboard_public_url);
      scheme = p.scheme; host = p.host; port = p.port;
    } else if (cf.mode === 'api_token' && cf.service_hosts && cf.service_hosts['dash']){
      scheme = 'https'; host = cf.service_hosts['dash']; port = '443';
    } else if (cf.mode === 'quick' && cf.quick_tunnel_urls && cf.quick_tunnel_urls['dash']){
      var pq = parseUrlParts(cf.quick_tunnel_urls['dash']);
      scheme = pq.scheme; host = pq.host; port = pq.port;
    } else {
      scheme = 'http'; host = window.location.hostname; port = localPort;
    }

    var portSuffix = (port && port !== '443' && port !== '80') ? (':' + port) : '';
    var localUrl = scheme + '://' + host + portSuffix + '/ui/';
    var fallbackUrl = 'https://metacubexd.pages.dev/#/setup?hostname=' + encodeURIComponent(host) + '&port=' + encodeURIComponent(port) + '&secret=' + encodeURIComponent(secret);

    var fallbackTimer = setTimeout(function(){
      showMessage('Local dashboard not reachable yet, falling back to metacubexd.pages.dev', 'success');
      frame.src = fallbackUrl;
    }, 4000);
    frame.onload = function(){ clearTimeout(fallbackTimer); };
    frame.src = localUrl;
  }

})();
</script>
</body>
</html>
`

// ---------------------------------------------------------------------
// توابع کمکی فایل (خواندن/نوشتن اتمیک)
// ---------------------------------------------------------------------
func readJSON(filename string, dest interface{}) error {
	data, err := os.ReadFile(filename)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, dest)
}

// atomicWriteFile محتوا را ابتدا در یک فایل موقت در همان دایرکتوری می‌نویسد
// و سپس با rename جایگزین فایل مقصد می‌کند تا هرگز یک فایل نصفه/خراب روی دیسک نماند.
func atomicWriteFile(filename string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(filename)
	if dir == "" {
		dir = "."
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // اگر rename موفق شود این یک no-op بی‌خطر است

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, filename)
}

func writeJSONAtomic(filename string, data interface{}) error {
	buf, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(filename, buf, 0644)
}

// ---------------------------------------------------------------------
// state.json — متادیتای داخلی خود مدیر (کدام گروه WARP پیش‌فرض است و غیره).
// این فایل هرگز به sing-box داده نمی‌شود، فقط برای منطق داخلی برنامه است.
// ---------------------------------------------------------------------
// DockerService یک وب‌سرویس جدا (کانتینر Docker با یک فرانت وب، مثل zai/grok/deepseek)
// را نشان می‌دهد که هم به‌صورت iframe در پنل نمایش داده می‌شود و هم (در حالت api_token)
// یک ساب‌دامین عمومی از طریق تونل Cloudflare می‌گیرد.
type DockerService struct {
	Name string `json:"name"`           // اسم/ساب‌دامین، مثل "zai"
	Port int    `json:"port"`           // پورتی که کانتینر روی هاست منتشر کرده، مثل 3000
	Path string `json:"path,omitempty"` // مسیر اختیاری، پیش‌فرض "/"
	// PublicURL آدرس عمومی این سرویس را دستی override می‌کند. لازم است وقتی مدیر
	// نمی‌تواند هاست‌نیم واقعی را حدس بزند: حالت tunnel_token (ingress دستی توسط
	// کاربر) یا هر ریورس‌پروکسی/تونل دیگری غیر از حالت api_token. اگر خالی باشد،
	// فرانت‌اند بر اساس mode فعلی تونل Cloudflare حدس می‌زند (به resolveServicePublicUrl
	// در htmlContent مراجعه کنید).
	PublicURL string `json:"public_url,omitempty"`
}

// CloudflareConfig تنظیمات تونل Cloudflare را نگه می‌دارد. سه حالت پشتیبانی می‌شود:
//   - api_token: کاربر یک API Token کامل (Tunnel:Edit + DNS:Edit) و یک دامنه‌ی خودش می‌دهد؛
//     مدیر خودکار تونل را می‌سازد، ingress را بر اساس DockerServices/پنل/داشبورد تنظیم می‌کند
//     و رکوردهای DNS را می‌سازد.
//   - tunnel_token: کاربر فقط یک Tunnel Token (از یک تونل که خودش در داشبورد Cloudflare
//     ساخته و route هایش را دستی تنظیم کرده) می‌دهد؛ مدیر فقط cloudflared را اجرا می‌کند.
//   - quick: کاربر هیچ‌کدام را ندارد؛ از Quick Tunnel رایگان *.trycloudflare.com استفاده
//     می‌شود (بدون نیاز به اکانت/دامنه)، فقط برای پنل مدیریت و داشبورد Clash.
type CloudflareConfig struct {
	Mode        string `json:"mode,omitempty"` // "api_token" | "tunnel_token" | "quick"
	APIToken    string `json:"api_token,omitempty"`
	ZoneName    string `json:"zone_name,omitempty"`
	ZoneID      string `json:"zone_id,omitempty"`
	AccountID   string `json:"account_id,omitempty"`
	TunnelToken string `json:"tunnel_token,omitempty"`
	TunnelID    string `json:"tunnel_id,omitempty"`
	TunnelName  string `json:"tunnel_name,omitempty"`
	// DashboardPublicURL فقط در حالت tunnel_token معنا دارد: چون ingress آنجا دستی
	// توسط خود کاربر در داشبورد Cloudflare تنظیم می‌شود، مدیر نمی‌تواند هاست‌نیم
	// عمومی Clash API/Dashboard را حدس بزند و کاربر باید آن را اینجا صریحاً بدهد
	// (مثلاً https://dash.example.com) تا داشبورد و metacubexd.pages.dev درست کار کنند.
	DashboardPublicURL string `json:"dashboard_public_url,omitempty"`
}

type AppState struct {
	DefaultWarpGroup string `json:"default_warp_group"`
	// AdminToken در صورت تنظیم از صفحه‌ی Settings، بر متغیر محیطی ADMIN_TOKEN اولویت دارد.
	AdminToken string `json:"admin_token,omitempty"`
	// Cloudflare تنظیمات تونل (هر سه حالت) را نگه می‌دارد.
	Cloudflare CloudflareConfig `json:"cloudflare,omitempty"`
	// DockerServices فهرست وب‌سرویس‌های جدا (zai/grok/deepseek و غیره) برای iframe + تونل.
	DockerServices []DockerService `json:"docker_services,omitempty"`
	// EnvOverrides مقادیر تنظیمات مبتنی‌بر env که از صفحه‌ی Settings تغییر داده شده‌اند
	// (اولویت با این مقادیر است، سپس متغیر محیطی واقعی، سپس مقدار پیش‌فرض کد).
	EnvOverrides map[string]string `json:"env_overrides,omitempty"`
}

func readStateOrDefault() AppState {
	var s AppState
	if err := readJSON(stateFile, &s); err != nil {
		return AppState{}
	}
	return s
}

func writeState(s AppState) error {
	return writeJSONAtomic(stateFile, s)
}

// ---------------------------------------------------------------------
// تضمین وجود template.json و nodes.json — با bootstrap کامل و خودکار
// در صورتی که *هیچ‌کدام* از این دو فایل وجود نداشته باشد (نصب تازه).
// ---------------------------------------------------------------------

// minimalDefaultTemplate تنها زمانی استفاده می‌شود که یکی از دو فایل (نه هر دو)
// از قبل موجود بوده و فقط دیگری گم شده — یک fallback امن و کمینه، بدون دست‌زدن
// به فایل دیگری که ادمین از قبل داشته است.
const minimalDefaultTemplate = `{
  "inbounds": [],
  "outbounds": [
    {
      "tag": "auto",
      "type": "urltest",
      "outbounds": [],
      "url": "http://www.gstatic.com/generate_204",
      "interval": "10m"
    },
    {
      "tag": "direct",
      "type": "direct"
    }
  ],
  "route": {
    "rules": []
  },
  "experimental": {
    "clash_api": {
      "external_controller": "127.0.0.1:__CLASH_API_PORT__",
      "external_ui": "ui",
      "external_ui_download_url": "https://github.com/MetaCubeX/metacubexd/archive/refs/heads/gh-pages.zip",
      "secret": "",
      "default_mode": "rule"
    }
  }
}`

// defaultTemplateRich پایه‌ی کامل و آماده‌ی تولید است — دقیقاً همان ساختاری که
// برای استقرار واقعی استفاده می‌شود (DNS، rule_setهای ir/ads/private، این‌باندهای
// global/auto/direct و ...). فقط در bootstrap نصب تازه به کار می‌رود.
// __CLASH_SECRET__ در زمان اجرا با یک secret تصادفی جایگزین می‌شود.
const defaultTemplateRich = `{
  "dns": {
    "final": "local-dns",
    "rules": [
      {
        "action": "route",
        "clash_mode": "Global",
        "server": "proxy-dns",
        "source_ip_cidr": [
          "172.19.0.0/30",
          "fdfe:dcba:9876::1/126"
        ]
      },
      {
        "action": "route",
        "server": "proxy-dns",
        "source_ip_cidr": [
          "172.19.0.0/30",
          "fdfe:dcba:9876::1/126"
        ]
      },
      {
        "action": "route",
        "clash_mode": "Direct",
        "server": "direct-dns"
      },
      {
        "action": "route",
        "rule_set": [
          "geosite-ir"
        ],
        "server": "direct-dns"
      }
    ],
    "servers": [
      {
        "detour": "proxy",
        "server": "1.1.1.1",
        "server_port": 53,
        "tag": "proxy-dns",
        "type": "tcp"
      },
      {
        "tag": "local-dns",
        "type": "local"
      },
      {
        "server": "8.8.8.8",
        "server_port": 53,
        "tag": "direct-dns",
        "type": "tcp"
      }
    ],
    "strategy": "prefer_ipv4"
  },
  "endpoints": [],
  "experimental": {
    "clash_api": {
      "access_control_allow_origin": [
        "*"
      ],
      "access_control_allow_private_network": true,
      "default_mode": "rule",
      "external_controller": "127.0.0.1:__CLASH_API_PORT__",
      "external_ui": "ui",
      "external_ui_download_url": "https://github.com/MetaCubeX/metacubexd/archive/refs/heads/gh-pages.zip",
      "secret": "__CLASH_SECRET__"
    }
  },
  "inbounds": [
    {
      "listen": "127.0.0.1",
      "listen_port": 2080,
      "tag": "in-global",
      "type": "mixed"
    },
    {
      "listen": "127.0.0.1",
      "listen_port": 2081,
      "tag": "in-auto",
      "type": "mixed"
    },
    {
      "listen": "127.0.0.1",
      "listen_port": 2082,
      "tag": "in-direct",
      "type": "mixed"
    }
  ],
  "outbounds": [
    {
      "outbounds": [
        "auto",
        "direct"
      ],
      "tag": "proxy",
      "type": "selector"
    },
    {
      "interval": "10m",
      "outbounds": [
        "direct"
      ],
      "tag": "auto",
      "tolerance": 50,
      "type": "urltest",
      "url": "http://www.gstatic.com/generate_204"
    },
    {
      "tag": "direct",
      "type": "direct"
    }
  ],
  "route": {
    "auto_detect_interface": true,
    "default_domain_resolver": "local-dns",
    "final": "proxy",
    "rule_set": [
      {
        "download_detour": "direct",
        "format": "binary",
        "tag": "geosite-ads",
        "type": "remote",
        "url": "https://raw.githubusercontent.com/itsyebekhe/meta-rules-dat-sing/main/geo/geosite/category-ads-all.srs"
      },
      {
        "download_detour": "direct",
        "format": "binary",
        "tag": "geosite-private",
        "type": "remote",
        "url": "https://raw.githubusercontent.com/itsyebekhe/meta-rules-dat-sing/main/geo/geosite/private.srs"
      },
      {
        "download_detour": "direct",
        "format": "binary",
        "tag": "geosite-ir",
        "type": "remote",
        "url": "https://raw.githubusercontent.com/itsyebekhe/meta-rules-dat-sing/main/geo/geosite/category-ir.srs"
      },
      {
        "download_detour": "direct",
        "format": "binary",
        "tag": "geoip-private",
        "type": "remote",
        "url": "https://raw.githubusercontent.com/itsyebekhe/meta-rules-dat-sing/main/geo/geoip/private.srs"
      },
      {
        "download_detour": "direct",
        "format": "binary",
        "tag": "geoip-ir",
        "type": "remote",
        "url": "https://raw.githubusercontent.com/itsyebekhe/meta-rules-dat-sing/main/geo/geoip/ir.srs"
      }
    ],
    "rules": [
      {
        "action": "sniff"
      },
      {
        "action": "route",
        "inbound": [
          "in-auto"
        ],
        "outbound": "auto"
      },
      {
        "action": "route",
        "inbound": [
          "in-direct"
        ],
        "outbound": "direct"
      },
      {
        "action": "route",
        "clash_mode": "Direct",
        "outbound": "direct"
      },
      {
        "action": "route",
        "clash_mode": "Global",
        "outbound": "proxy"
      },
      {
        "action": "hijack-dns",
        "protocol": "dns"
      },
      {
        "action": "route",
        "outbound": "direct",
        "rule_set": [
          "geoip-private",
          "geosite-private",
          "geosite-ir",
          "geoip-ir"
        ]
      },
      {
        "action": "reject",
        "rule_set": [
          "geosite-ads"
        ]
      }
    ]
  }
}`

func ensureDefaultFiles() {
	_, tmplErr := os.Stat(templateFile)
	_, nodesErr := os.Stat(nodesFile)

	if os.IsNotExist(tmplErr) && os.IsNotExist(nodesErr) {
		bootstrapFreshInstall()
		return
	}
	if os.IsNotExist(tmplErr) {
		if err := os.WriteFile(templateFile, []byte(minimalDefaultTemplate), 0644); err != nil {
			log.Printf("Failed to create default template.json: %v", err)
		} else {
			log.Printf("Created minimal default template.json (nodes.json already existed)")
		}
	}
	if os.IsNotExist(nodesErr) {
		if err := os.WriteFile(nodesFile, []byte("[]"), 0644); err != nil {
			log.Printf("Failed to create default nodes.json: %v", err)
		} else {
			log.Printf("Created empty nodes.json (template.json already existed)")
		}
	}
}

// defaultServiceDef یک سرویس پیش‌فرض (نام + پورت) است که در نصب تازه ساخته می‌شود.
type defaultServiceDef struct {
	Name string
	Port int
}

// parseDefaultServices لیست سرویس‌های پیش‌فرض را از متغیر محیطی DEFAULT_SERVICES
// می‌خواند، با فرمت "name:port,name2:port2" — مثلاً "telegram:2083,youtube:2084".
// اگر تنظیم نشده باشد، هیچ سرویس پیش‌فرضی ساخته نمی‌شود (فقط این‌باندهای پایه‌ی
// خود template مثل in-global/in-auto/in-direct فعال خواهند بود).
func parseDefaultServices() []defaultServiceDef {
	raw := strings.TrimSpace(os.Getenv("DEFAULT_SERVICES"))
	if raw == "" {
		return nil
	}
	var defs []defaultServiceDef
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kv := strings.SplitN(part, ":", 2)
		if len(kv) != 2 {
			log.Printf("DEFAULT_SERVICES: skipping malformed entry %q (expected name:port)", part)
			continue
		}
		name := strings.TrimSpace(kv[0])
		port, err := strconv.Atoi(strings.TrimSpace(kv[1]))
		if err != nil || !serviceNameRe.MatchString(name) || port < 1 || port > 65535 {
			log.Printf("DEFAULT_SERVICES: skipping invalid entry %q", part)
			continue
		}
		defs = append(defs, defaultServiceDef{Name: name, Port: port})
	}
	return defs
}

// bootstrapFreshInstall زمانی اجرا می‌شود که هیچ‌کدام از template.json/nodes.json
// وجود نداشته باشند: یک قالب کامل می‌سازد، یک اکانت WARP واقعی خودکار ثبت‌نام
// می‌کند (که همان گروه WARP پیش‌فرض می‌شود)، و هر سرویس پیش‌فرض تعریف‌شده در
// DEFAULT_SERVICES را با outbound پیش‌فرض به‌سوی Auto همان گروه می‌سازد.
func bootstrapFreshInstall() {
	log.Println("No existing template.json/nodes.json found — bootstrapping a fresh default setup")

	clashSecret := randomString(24)
	tmplStr := strings.Replace(defaultTemplateRich, "__CLASH_SECRET__", clashSecret, 1)
	tmplStr = strings.Replace(tmplStr, "__CLASH_API_PORT__", getEnvDefault("CLASH_API_PORT", "9090"), 1)

	var tmpl map[string]interface{}
	if err := json.Unmarshal([]byte(tmplStr), &tmpl); err != nil {
		log.Printf("bootstrap: default template is invalid JSON (this is a bug): %v", err)
		return
	}

	var nodes []interface{}
	state := AppState{}

	account, err := RegisterWarpAccount()
	if err != nil {
		log.Printf("bootstrap: could not auto-register a WARP account (%v) — starting with zero WARP nodes; add one from the WARP Nodes tab once the manager is up", err)
	} else {
		configs, genErr := GenerateWireGuardConfigs("WARP", account, warpEndpoints)
		if genErr != nil {
			log.Printf("bootstrap: failed to generate WARP endpoint configs: %v", genErr)
		} else {
			for _, cfg := range configs {
				m := map[string]interface{}{}
				b, _ := json.Marshal(cfg)
				_ = json.Unmarshal(b, &m)
				nodes = append(nodes, m)
			}
			state.DefaultWarpGroup = "WARP"
			log.Printf("Registered a default WARP account and generated %d endpoint node(s) under tag prefix \"WARP\"", len(configs))
		}
	}

	defaultTarget := "auto"
	if state.DefaultWarpGroup != "" {
		defaultTarget = state.DefaultWarpGroup + "-auto"
	}
	for _, svc := range parseDefaultServices() {
		if err := addServiceToTemplate(tmpl, svc.Name, svc.Port, defaultTarget); err != nil {
			log.Printf("bootstrap: could not add default service %q: %v", svc.Name, err)
		} else {
			log.Printf("Created default service %q on port %d (default outbound: %s)", svc.Name, svc.Port, defaultTarget)
		}
	}

	if err := writeState(state); err != nil {
		log.Printf("bootstrap: failed to write state.json: %v", err)
	}

	nodesRaw, err := json.MarshalIndent(nodes, "", "  ")
	if err != nil || len(nodes) == 0 {
		nodesRaw = []byte("[]")
	}
	if err := os.WriteFile(nodesFile, nodesRaw, 0644); err != nil {
		log.Printf("bootstrap: failed to write nodes.json: %v", err)
		return
	}

	tmplRaw, err := json.MarshalIndent(tmpl, "", "  ")
	if err != nil {
		log.Printf("bootstrap: failed to marshal final template: %v", err)
		// حداقل نسخه‌ی بدون سرویس‌های پیش‌فرض را روی دیسک نگه می‌داریم
		tmplRaw = []byte(tmplStr)
	}
	if err := os.WriteFile(templateFile, tmplRaw, 0644); err != nil {
		log.Printf("bootstrap: failed to persist template.json: %v", err)
		return
	}

	log.Println("Bootstrap complete — template.json and nodes.json are ready")
}

// ---------------------------------------------------------------------
// یافتن مسیر sing-box
// ---------------------------------------------------------------------
var (
	singBoxPathCache string
	singBoxPathMu    sync.Mutex
)

// findSingBox مسیر باینری sing-box را پیدا می‌کند و نتیجه را کش می‌کند.
// ترتیب جستجو: SINGBOX_PATH -> PATH -> دایرکتوری جاری -> مسیرهای رایج نصب.
func findSingBox() (string, error) {
	singBoxPathMu.Lock()
	defer singBoxPathMu.Unlock()

	if singBoxPathCache != "" {
		if info, err := os.Stat(singBoxPathCache); err == nil && !info.IsDir() {
			return singBoxPathCache, nil
		}
		singBoxPathCache = "" // دیگر معتبر نیست، دوباره جستجو کن
	}

	path, err := locateSingBox()
	if err != nil {
		return "", err
	}
	singBoxPathCache = path
	return path, nil
}

func locateSingBox() (string, error) {
	names := []string{"sing-box"}
	if runtime.GOOS == "windows" {
		names = []string{"sing-box.exe", "sing-box.bat", "sing-box.cmd"}
	}

	// ۱. اگر صریحاً با متغیر محیطی مشخص شده باشد
	if envPath := strings.TrimSpace(getSetting("SINGBOX_PATH", "")); envPath != "" {
		if info, err := os.Stat(envPath); err == nil && !info.IsDir() {
			log.Printf("Using sing-box from SINGBOX_PATH: %s", envPath)
			return envPath, nil
		}
		log.Printf("SINGBOX_PATH=%q is set but no executable was found there", envPath)
	}

	// ۲. در PATH سیستم
	for _, name := range names {
		if p, err := exec.LookPath(name); err == nil {
			log.Printf("Found sing-box in PATH: %s", p)
			return p, nil
		}
	}

	// ۳. دایرکتوری جاری
	cwd, _ := os.Getwd()
	searchDirs := []string{cwd, "."}

	// ۴. مسیرهای رایج نصب
	commonDirs := []string{
		"/usr/bin", "/usr/local/bin", "/usr/local/sbin", "/usr/sbin",
		"/opt/sing-box", "/root/sing-box",
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		commonDirs = append(commonDirs, home, filepath.Join(home, "sing-box"), filepath.Join(home, "go", "bin"))
	}
	searchDirs = append(searchDirs, commonDirs...)

	for _, dir := range searchDirs {
		for _, name := range names {
			candidate := filepath.Join(dir, name)
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				log.Printf("Found sing-box at: %s", candidate)
				return candidate, nil
			}
		}
	}

	return "", fmt.Errorf(
		"sing-box executable not found (checked PATH, working directory, and common install paths). " +
			"Install sing-box or set the SINGBOX_PATH environment variable to its full path, " +
			"e.g. SINGBOX_PATH=/usr/local/bin/sing-box",
	)
}

// ---------------------------------------------------------------------
// دانلود خودکار sing-box (در صورت نبود روی سیستم)
//
// اگر باینری sing-box پیدا نشود، این بخش نسخه‌ی مناسب سیستم‌عامل/معماری فعلی
// را مستقیماً از GitHub Releases دانلود می‌کند. لیست assetها (حدود ۱۵۰ فایل به
// ازای هر ریلیز، یکی برای هر ترکیب OS/arch) از GitHub API خوانده می‌شود تا به
// نام‌گذاری دقیق فایل‌ها در هر نسخه وابسته نباشیم.
// ---------------------------------------------------------------------
const (
	defaultSingBoxVersion  = "v1.13.16"
	singBoxReleaseAPI      = "https://api.github.com/repos/SagerNet/sing-box/releases/tags/"
	singBoxAPITimeout      = 15 * time.Second
	singBoxDownloadTimeout = 180 * time.Second
)

var singBoxDownloadHTTPClient = &http.Client{Timeout: singBoxDownloadTimeout}

type githubReleaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type githubRelease struct {
	TagName string               `json:"tag_name"`
	Assets  []githubReleaseAsset `json:"assets"`
}

// singBoxOSToken نام سیستم‌عامل را همان‌طور که در نام فایل‌های ریلیز sing-box
// استفاده می‌شود برمی‌گرداند.
func singBoxOSToken() (string, error) {
	switch runtime.GOOS {
	case "linux", "windows", "darwin", "freebsd":
		return runtime.GOOS, nil
	default:
		return "", fmt.Errorf("automatic sing-box download is not supported on %s", runtime.GOOS)
	}
}

// singBoxArchTokens بر اساس runtime.GOARCH فعلی، توکن‌های معماری را به ترتیب
// اولویت برمی‌گرداند (مثلاً amd64 هم به‌صورت ساده و هم به‌صورت amd64v3 منتشر می‌شود).
func singBoxArchTokens() []string {
	switch runtime.GOARCH {
	case "amd64":
		return []string{"amd64", "amd64v3"}
	case "arm64":
		return []string{"arm64"}
	case "386":
		return []string{"386"}
	case "arm":
		return []string{"armv7", "armv6", "armv5"}
	case "mips":
		return []string{"mips-hardfloat", "mips-softfloat", "mips"}
	case "mipsle":
		return []string{"mipsle-hardfloat", "mipsle-softfloat", "mipsle"}
	case "mips64":
		return []string{"mips64"}
	case "mips64le":
		return []string{"mips64le"}
	case "riscv64":
		return []string{"riscv64"}
	case "s390x":
		return []string{"s390x"}
	default:
		return []string{runtime.GOARCH}
	}
}

// fetchSingBoxRelease اطلاعات یک ریلیز مشخص (شامل لیست assetها) را از GitHub API می‌گیرد.
func fetchSingBoxRelease(version string) (*githubRelease, error) {
	version = strings.TrimSpace(version)
	if version == "" {
		version = defaultSingBoxVersion
	}
	if !strings.HasPrefix(version, "v") {
		version = "v" + version
	}

	ctx, cancel := context.WithTimeout(context.Background(), singBoxAPITimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, singBoxReleaseAPI+version, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "sb-manager")

	resp, err := singBoxDownloadHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to reach GitHub: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("sing-box release %s was not found", version)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GitHub API returned status %d: %s", resp.StatusCode, string(body))
	}

	var rel githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("failed to decode GitHub response: %w", err)
	}
	return &rel, nil
}

// selectSingBoxAsset از بین تمام assetهای یک ریلیز (فایل باینری هر OS/arch، سورس،
// چک‌سام‌ها، apk اندروید و غیره) دقیقاً همان آرشیوی را انتخاب می‌کند که با سیستم‌عامل
// و معماری فعلی مطابقت دارد.
func selectSingBoxAsset(rel *githubRelease, osToken string, archTokens []string) (*githubReleaseAsset, error) {
	ext := ".tar.gz"
	if osToken == "windows" {
		ext = ".zip"
	}
	for _, arch := range archTokens {
		suffix := "-" + osToken + "-" + arch + ext
		for i := range rel.Assets {
			a := &rel.Assets[i]
			if strings.HasPrefix(a.Name, "sing-box-") && strings.HasSuffix(a.Name, suffix) {
				return a, nil
			}
		}
	}
	return nil, fmt.Errorf("no matching sing-box asset found for %s/%s in release %s (out of %d assets)", osToken, runtime.GOARCH, rel.TagName, len(rel.Assets))
}

// extractFromTarGz فایل باینری binName را از داخل یک آرشیو .tar.gz در حافظه استخراج می‌کند.
func extractFromTarGz(data []byte, binName string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("failed to open gzip archive: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read tar archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg || filepath.Base(hdr.Name) != binName {
			continue
		}
		out, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("failed to read %s from archive: %w", binName, err)
		}
		return out, nil
	}
	return nil, fmt.Errorf("%s not found inside the downloaded archive", binName)
}

// extractFromZip فایل باینری binName را از داخل یک آرشیو .zip در حافظه استخراج می‌کند.
func extractFromZip(data []byte, binName string) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("failed to open zip archive: %w", err)
	}
	for _, f := range zr.File {
		if filepath.Base(f.Name) != binName {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("failed to open %s in archive: %w", binName, err)
		}
		out, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("failed to read %s from archive: %w", binName, err)
		}
		return out, nil
	}
	return nil, fmt.Errorf("%s not found inside the downloaded archive", binName)
}

// downloadAndExtractArchive یک آرشیو (.zip یا .tar.gz) را دانلود می‌کند، فایل binName
// را از داخل آن استخراج و در destPath (با مجوز اجرا) می‌نویسد. برای sing-box و
// cloudflared (نسخه‌ی macOS) هر دو استفاده می‌شود.
func downloadAndExtractArchive(url, destPath, binName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), singBoxDownloadTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "sb-manager")

	resp, err := singBoxDownloadHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to download %s: %w", binName, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed with status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read downloaded archive: %w", err)
	}

	var binData []byte
	if strings.HasSuffix(url, ".zip") {
		binData, err = extractFromZip(body, binName)
	} else {
		binData, err = extractFromTarGz(body, binName)
	}
	if err != nil {
		return err
	}

	if dir := filepath.Dir(destPath); dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create destination directory: %w", err)
		}
	}
	if err := atomicWriteFile(destPath, binData, 0755); err != nil {
		return fmt.Errorf("failed to write sing-box binary: %w", err)
	}
	return nil
}

// singBoxInstallDir مسیری است که باینری دانلودشده‌ی sing-box در آن نوشته می‌شود.
func singBoxInstallDir() string {
	if dir := strings.TrimSpace(getSetting("SINGBOX_INSTALL_DIR", "")); dir != "" {
		return dir
	}
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	return filepath.Join(cwd, "bin")
}

// downloadSingBox نسخه‌ی مشخص‌شده (یا پیش‌فرض) از sing-box را برای سیستم‌عامل/معماری
// فعلی دانلود و نصب می‌کند، کش مسیر باینری را به‌روز می‌کند و مسیر نهایی را برمی‌گرداند.
func downloadSingBox(version string) (string, error) {
	if strings.TrimSpace(version) == "" {
		version = getSetting("SINGBOX_VERSION", defaultSingBoxVersion)
	}

	osToken, err := singBoxOSToken()
	if err != nil {
		return "", err
	}
	rel, err := fetchSingBoxRelease(version)
	if err != nil {
		return "", err
	}
	asset, err := selectSingBoxAsset(rel, osToken, singBoxArchTokens())
	if err != nil {
		return "", err
	}

	binName := "sing-box"
	if osToken == "windows" {
		binName = "sing-box.exe"
	}
	destPath := filepath.Join(singBoxInstallDir(), binName)

	log.Printf("Downloading sing-box %s (%s) from %s", rel.TagName, asset.Name, asset.BrowserDownloadURL)
	if err := downloadAndExtractArchive(asset.BrowserDownloadURL, destPath, binName); err != nil {
		return "", err
	}
	log.Printf("sing-box %s installed at %s", rel.TagName, destPath)

	singBoxPathMu.Lock()
	singBoxPathCache = destPath
	singBoxPathMu.Unlock()

	return destPath, nil
}

// autoDownloadSingBoxIfMissing در استارتاپ فراخوانی می‌شود: اگر sing-box از قبل
// روی سیستم پیدا نشود، تلاش می‌کند نسخه‌ی پیش‌فرض (یا SINGBOX_VERSION) را خودکار
// دانلود کند. با SINGBOX_NO_AUTO_DOWNLOAD=1 می‌توان این رفتار را غیرفعال کرد.
func autoDownloadSingBoxIfMissing() {
	if strings.TrimSpace(getSetting("SINGBOX_NO_AUTO_DOWNLOAD", "")) != "" {
		return
	}
	if _, err := findSingBox(); err == nil {
		return
	}
	version := getSetting("SINGBOX_VERSION", defaultSingBoxVersion)
	log.Printf("sing-box executable not found — attempting automatic download of %s for %s/%s", version, runtime.GOOS, runtime.GOARCH)
	if _, err := downloadSingBox(version); err != nil {
		log.Printf("automatic sing-box download failed: %v (retry from the Settings page, install it manually, or set SINGBOX_PATH)", err)
		return
	}
	log.Println("sing-box downloaded and ready")
}

// ---------------------------------------------------------------------
// مدیریت پروسه‌ی sing-box (استارت/استاپ/ری‌استارت واقعی)
// ---------------------------------------------------------------------
type managedProcess struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

var (
	singBoxCmdMu   sync.Mutex
	runningSingBox *managedProcess
)

func startSingBoxLocked(path string) (*managedProcess, error) {
	cmd := exec.Command(path, "run", "-c", configFile)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start sing-box: %w", err)
	}
	mp := &managedProcess{cmd: cmd, done: make(chan struct{})}
	go func() {
		mp.err = cmd.Wait()
		close(mp.done)
	}()
	log.Printf("sing-box started (pid=%d)", cmd.Process.Pid)
	return mp, nil
}

// stopProcess به‌آرامی پروسه را متوقف می‌کند (با Interrupt) و در صورت timeout آن را Kill می‌کند.
// Wait() فقط یک‌بار و فقط توسط گوروتین راه‌اندازی‌شده در startSingBoxLocked فراخوانی می‌شود.
func stopProcess(mp *managedProcess) {
	if mp == nil || mp.cmd == nil || mp.cmd.Process == nil {
		return
	}
	_ = mp.cmd.Process.Signal(os.Interrupt)
	select {
	case <-mp.done:
	case <-time.After(singBoxStopTimeout):
		log.Printf("sing-box did not exit within %s, killing it", singBoxStopTimeout)
		_ = mp.cmd.Process.Kill()
		<-mp.done
	}
	if mp.err != nil {
		log.Printf("sing-box process exited: %v", mp.err)
	} else {
		log.Printf("sing-box process exited cleanly")
	}
}

// restartSingBox پروسه‌ی قبلی (در صورت وجود) را متوقف کرده و یک نمونه‌ی جدید
// با config.json فعلی اجرا می‌کند.
func restartSingBox() error {
	singBoxCmdMu.Lock()
	defer singBoxCmdMu.Unlock()

	path, err := findSingBox()
	if err != nil {
		return err
	}

	stopProcess(runningSingBox)
	runningSingBox = nil

	mp, err := startSingBoxLocked(path)
	if err != nil {
		return err
	}
	runningSingBox = mp
	return nil
}

// ---------------------------------------------------------------------
// دانلود خودکار باینری cloudflared (الگوی مشابه دانلود sing-box، اما نام‌گذاری
// assetهای cloudflared فرق دارد: لینوکس/ویندوز باینری خام هستند، macOS در tar.gz)
// ---------------------------------------------------------------------
const (
	cloudflaredRepoAPI     = "https://api.github.com/repos/cloudflare/cloudflared/releases/latest"
	cloudflaredStopTimeout = 10 * time.Second
)

// cloudflaredAssetName نام asset مناسب سیستم‌عامل/معماری فعلی را در ریلیزهای
// cloudflared برمی‌گرداند (بر اساس الگوی نام‌گذاری رسمی این پروژه).
func cloudflaredAssetName() (name string, isArchive bool, err error) {
	switch runtime.GOOS {
	case "linux":
		switch runtime.GOARCH {
		case "amd64":
			return "cloudflared-linux-amd64", false, nil
		case "arm64":
			return "cloudflared-linux-arm64", false, nil
		case "386":
			return "cloudflared-linux-386", false, nil
		case "arm":
			return "cloudflared-linux-arm", false, nil
		}
	case "windows":
		switch runtime.GOARCH {
		case "amd64":
			return "cloudflared-windows-amd64.exe", false, nil
		case "386":
			return "cloudflared-windows-386.exe", false, nil
		}
	case "darwin":
		switch runtime.GOARCH {
		case "amd64":
			return "cloudflared-darwin-amd64.tgz", true, nil
		case "arm64":
			return "cloudflared-darwin-arm64.tgz", true, nil
		}
	}
	return "", false, fmt.Errorf("automatic cloudflared download is not supported on %s/%s — install it manually", runtime.GOOS, runtime.GOARCH)
}

func cloudflaredInstallDir() string {
	if dir := strings.TrimSpace(getSetting("CLOUDFLARED_INSTALL_DIR", "")); dir != "" {
		return dir
	}
	return singBoxInstallDir() // همان دایرکتوری bin استفاده‌شده برای sing-box
}

// downloadCloudflared آخرین نسخه‌ی cloudflared را برای سیستم‌عامل/معماری فعلی
// دانلود می‌کند. لینوکس/ویندوز باینری خام‌اند، macOS داخل tar.gz است.
func downloadCloudflared() (string, error) {
	assetName, isArchive, err := cloudflaredAssetName()
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), singBoxAPITimeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cloudflaredRepoAPI, nil)
	if err != nil {
		cancel()
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "sb-manager")
	resp, err := singBoxDownloadHTTPClient.Do(req)
	cancel()
	if err != nil {
		return "", fmt.Errorf("failed to reach GitHub: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("GitHub API returned status %d: %s", resp.StatusCode, string(body))
	}
	var rel githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", fmt.Errorf("failed to decode GitHub response: %w", err)
	}

	var asset *githubReleaseAsset
	for i := range rel.Assets {
		if rel.Assets[i].Name == assetName {
			asset = &rel.Assets[i]
			break
		}
	}
	if asset == nil {
		return "", fmt.Errorf("asset %q not found in cloudflared release %s", assetName, rel.TagName)
	}

	binName := "cloudflared"
	if runtime.GOOS == "windows" {
		binName = "cloudflared.exe"
	}
	destPath := filepath.Join(cloudflaredInstallDir(), binName)

	log.Printf("Downloading cloudflared %s (%s) from %s", rel.TagName, asset.Name, asset.BrowserDownloadURL)

	if !isArchive {
		// لینوکس/ویندوز: باینری خام است، مستقیم دانلود و ذخیره می‌شود.
		dctx, dcancel := context.WithTimeout(context.Background(), singBoxDownloadTimeout)
		defer dcancel()
		dreq, err := http.NewRequestWithContext(dctx, http.MethodGet, asset.BrowserDownloadURL, nil)
		if err != nil {
			return "", err
		}
		dreq.Header.Set("User-Agent", "sb-manager")
		dresp, err := singBoxDownloadHTTPClient.Do(dreq)
		if err != nil {
			return "", fmt.Errorf("failed to download cloudflared: %w", err)
		}
		defer dresp.Body.Close()
		if dresp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("download failed with status %d", dresp.StatusCode)
		}
		body, err := io.ReadAll(dresp.Body)
		if err != nil {
			return "", fmt.Errorf("failed to read download: %w", err)
		}
		if dir := filepath.Dir(destPath); dir != "" {
			_ = os.MkdirAll(dir, 0755)
		}
		if err := atomicWriteFile(destPath, body, 0755); err != nil {
			return "", fmt.Errorf("failed to write cloudflared binary: %w", err)
		}
	} else {
		// macOS: asset یک tar.gz حاوی باینری cloudflared است.
		if err := downloadAndExtractArchive(asset.BrowserDownloadURL, destPath, binName); err != nil {
			return "", err
		}
	}

	log.Printf("cloudflared %s installed at %s", rel.TagName, destPath)
	cloudflaredPathMu.Lock()
	cloudflaredPathCache = destPath
	cloudflaredPathMu.Unlock()
	return destPath, nil
}

var (
	cloudflaredPathCache string
	cloudflaredPathMu    sync.Mutex
)

// findCloudflared مسیر باینری cloudflared را جستجو می‌کند (کش، CLOUDFLARED_PATH، PATH،
// دایرکتوری کاری، و مسیرهای رایج نصب) — دقیقاً هم‌الگو با findSingBox.
func findCloudflared() (string, error) {
	cloudflaredPathMu.Lock()
	defer cloudflaredPathMu.Unlock()

	if cloudflaredPathCache != "" {
		if info, err := os.Stat(cloudflaredPathCache); err == nil && !info.IsDir() {
			return cloudflaredPathCache, nil
		}
		cloudflaredPathCache = ""
	}

	if envPath := strings.TrimSpace(getSetting("CLOUDFLARED_PATH", "")); envPath != "" {
		if info, err := os.Stat(envPath); err == nil && !info.IsDir() {
			cloudflaredPathCache = envPath
			return envPath, nil
		}
	}

	binName := "cloudflared"
	if runtime.GOOS == "windows" {
		binName = "cloudflared.exe"
	}
	if p, err := exec.LookPath(binName); err == nil {
		cloudflaredPathCache = p
		return p, nil
	}
	candidates := []string{
		filepath.Join(cloudflaredInstallDir(), binName),
		"./" + binName,
		"/usr/local/bin/" + binName,
		"/usr/bin/" + binName,
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			cloudflaredPathCache = c
			return c, nil
		}
	}
	return "", fmt.Errorf("cloudflared executable not found (checked PATH, working directory, and common install paths)")
}

// ---------------------------------------------------------------------
// کلاینت Cloudflare API (حالت api_token): resolve zone/account، ساخت تونل،
// گرفتن run-token، push کردن ingress، و ساخت رکورد DNS.
// ---------------------------------------------------------------------
const cfAPIBase = "https://api.cloudflare.com/client/v4"

var cfHTTPClient = &http.Client{Timeout: 20 * time.Second}

func cfAPIRequest(token, method, path string, body interface{}) (map[string]interface{}, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, cfAPIBase+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := cfHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to reach Cloudflare API: %w", err)
	}
	defer resp.Body.Close()
	var parsed map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("failed to decode Cloudflare API response: %w", err)
	}
	if ok, _ := parsed["success"].(bool); !ok {
		return parsed, fmt.Errorf("Cloudflare API error (status %d): %v", resp.StatusCode, parsed["errors"])
	}
	return parsed, nil
}

// cfResolveZone نام دامنه را به zone_id و account_id آن تبدیل می‌کند.
func cfResolveZone(token, zoneName string) (zoneID, accountID string, err error) {
	res, err := cfAPIRequest(token, http.MethodGet, "/zones?name="+strings.TrimSpace(zoneName), nil)
	if err != nil {
		return "", "", err
	}
	results, _ := res["result"].([]interface{})
	if len(results) == 0 {
		return "", "", fmt.Errorf("zone %q not found in this Cloudflare account (check the domain name and API Token permissions)", zoneName)
	}
	zone, _ := results[0].(map[string]interface{})
	zoneID, _ = zone["id"].(string)
	account, _ := zone["account"].(map[string]interface{})
	accountID, _ = account["id"].(string)
	if zoneID == "" || accountID == "" {
		return "", "", fmt.Errorf("could not determine zone/account id for %q", zoneName)
	}
	return zoneID, accountID, nil
}

// cfEnsureTunnel یک تونل remotely-managed می‌سازد (یا اگر از قبل در state.json ذخیره
// شده، از همان استفاده می‌کند) و run-token آن را برمی‌گرداند.
func cfEnsureTunnel(token, accountID string, existing CloudflareConfig) (tunnelID, tunnelToken string, err error) {
	if existing.TunnelID != "" {
		// تونل قبلاً ساخته شده؛ فقط توکن اجرا را دوباره می‌گیریم (ممکن است چرخیده باشد).
		res, terr := cfAPIRequest(token, http.MethodGet, "/accounts/"+accountID+"/cfd_tunnel/"+existing.TunnelID+"/token", nil)
		if terr == nil {
			if tok, ok := res["result"].(string); ok && tok != "" {
				return existing.TunnelID, tok, nil
			}
		}
		log.Printf("cfEnsureTunnel: could not reuse existing tunnel %s (%v), creating a new one", existing.TunnelID, terr)
	}

	name := existing.TunnelName
	if name == "" {
		name = "sb-manager"
	}
	created, err := cfAPIRequest(token, http.MethodPost, "/accounts/"+accountID+"/cfd_tunnel", map[string]interface{}{
		"name":       name,
		"config_src": "cloudflare", // یعنی ingress از طریق API مدیریت می‌شود، نه فایل محلی
	})
	if err != nil {
		return "", "", fmt.Errorf("failed to create tunnel: %w", err)
	}
	result, _ := created["result"].(map[string]interface{})
	tunnelID, _ = result["id"].(string)
	if tunnelID == "" {
		return "", "", fmt.Errorf("tunnel creation response did not include an id")
	}

	tokRes, err := cfAPIRequest(token, http.MethodGet, "/accounts/"+accountID+"/cfd_tunnel/"+tunnelID+"/token", nil)
	if err != nil {
		return "", "", fmt.Errorf("failed to fetch tunnel run-token: %w", err)
	}
	tunnelToken, _ = tokRes["result"].(string)
	if tunnelToken == "" {
		return "", "", fmt.Errorf("tunnel token response was empty")
	}
	return tunnelID, tunnelToken, nil
}

// ingressRoute یک قانون ingress در تونل Cloudflare (هاست‌نیم عمومی → آدرس محلی).
type ingressRoute struct {
	// Key شناسه‌ی منطقی این route است: "panel"، "dash"، یا اسم Docker service —
	// برای اینکه فرانت‌اند بتواند هاست‌نیم درست هر سرویس را بدون duplicate کردن
	// منطق ساخت ساب‌دامین (service.Name + "." + domain) از API بخواند.
	Key      string
	Hostname string
	Service  string
}

// getClashAPIAddr آدرس external_controller فعلی را از template.json می‌خواند
// (به‌جای هاردکد 9090)، تا داشبورد Clash همیشه پورت درست را تونل کند.
func getClashAPIAddr() (string, error) {
	var tmpl map[string]interface{}
	if err := readJSON(templateFile, &tmpl); err != nil {
		return "127.0.0.1:9090", nil // fallback به پیش‌فرض اگر template.json هنوز آماده نیست
	}
	exp, _ := tmpl["experimental"].(map[string]interface{})
	if exp == nil {
		return "127.0.0.1:9090", nil
	}
	clash, _ := exp["clash_api"].(map[string]interface{})
	if clash == nil {
		return "127.0.0.1:9090", nil
	}
	if addr, ok := clash["external_controller"].(string); ok && addr != "" {
		return addr, nil
	}
	return "127.0.0.1:9090", nil
}

// computeTunnelRoutes فهرست ingress را می‌سازد: پنل مدیریت + داشبورد Clash + همه‌ی
// DockerServiceها (zai/grok/deepseek و غیره). عمداً پروکسی‌های جدول Services
// (سینگ‌باکس mixed inbound) اینجا نیستند — طبق تصمیم صریح شما فقط پروکسی‌ها private می‌مانند.
func computeTunnelRoutes(state AppState) []ingressRoute {
	domain := strings.TrimSuffix(strings.TrimSpace(state.Cloudflare.ZoneName), ".")
	if domain == "" {
		return nil
	}
	routes := []ingressRoute{
		{Key: "panel", Hostname: "panel." + domain, Service: "http://127.0.0.1" + apiPort},
	}
	if clashAddr, err := getClashAPIAddr(); err == nil {
		routes = append(routes, ingressRoute{Key: "dash", Hostname: "dash." + domain, Service: "http://" + clashAddr})
	}
	for _, s := range state.DockerServices {
		routes = append(routes, ingressRoute{
			Key:      s.Name,
			Hostname: s.Name + "." + domain,
			Service:  fmt.Sprintf("http://127.0.0.1:%d", s.Port),
		})
	}
	return routes
}

// cfPushIngress قوانین ingress را روی تونل remotely-managed تنظیم می‌کند.
func cfPushIngress(token, accountID, tunnelID string, routes []ingressRoute) error {
	ingress := make([]map[string]interface{}, 0, len(routes)+1)
	for _, r := range routes {
		ingress = append(ingress, map[string]interface{}{"hostname": r.Hostname, "service": r.Service})
	}
	ingress = append(ingress, map[string]interface{}{"service": "http_status:404"}) // catch-all اجباری
	_, err := cfAPIRequest(token, http.MethodPut, "/accounts/"+accountID+"/cfd_tunnel/"+tunnelID+"/configurations", map[string]interface{}{
		"config": map[string]interface{}{"ingress": ingress},
	})
	return err
}

// cfUpsertDNS برای هر route یک رکورد CNAME به‌سمت <tunnel_id>.cfargotunnel.com می‌سازد
// (اگر از قبل رکوردی با همان اسم باشد، آن را به‌روزرسانی می‌کند).
func cfUpsertDNS(token, zoneID, tunnelID string, routes []ingressRoute) error {
	target := tunnelID + ".cfargotunnel.com"
	for _, r := range routes {
		existing, err := cfAPIRequest(token, http.MethodGet, "/zones/"+zoneID+"/dns_records?type=CNAME&name="+r.Hostname, nil)
		if err != nil {
			log.Printf("cfUpsertDNS: failed to look up %s: %v", r.Hostname, err)
			continue
		}
		results, _ := existing["result"].([]interface{})
		body := map[string]interface{}{"type": "CNAME", "name": r.Hostname, "content": target, "proxied": true, "ttl": 1}
		if len(results) > 0 {
			rec, _ := results[0].(map[string]interface{})
			recID, _ := rec["id"].(string)
			if _, err := cfAPIRequest(token, http.MethodPut, "/zones/"+zoneID+"/dns_records/"+recID, body); err != nil {
				log.Printf("cfUpsertDNS: failed to update %s: %v", r.Hostname, err)
			}
			continue
		}
		if _, err := cfAPIRequest(token, http.MethodPost, "/zones/"+zoneID+"/dns_records", body); err != nil {
			log.Printf("cfUpsertDNS: failed to create %s: %v", r.Hostname, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------
// مدیریت پروسه‌ی cloudflared (سه حالت: api_token / tunnel_token / quick)
// ---------------------------------------------------------------------
var (
	cfCmdMu          sync.Mutex
	runningCF        *managedProcess
	cfQuickURLsMu    sync.RWMutex
	cfQuickURLs      = map[string]string{} // label -> https://xxxx.trycloudflare.com
	cfQuickProcesses []*managedProcess
)

// startCloudflaredWithToken یک تونل remotely-managed (api_token یا tunnel_token) را
// اجرا می‌کند. توکن عمداً به‌جای آرگومان CLI (که با ps قابل‌دیدن است) از طریق
// متغیر محیطی TUNNEL_TOKEN که خود cloudflared پشتیبانی می‌کند پاس داده می‌شود.
func startCloudflaredWithToken(path, token string) (*managedProcess, error) {
	cmd := exec.Command(path, "tunnel", "--no-autoupdate", "run")
	cmd.Env = append(os.Environ(), "TUNNEL_TOKEN="+token)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start cloudflared: %w", err)
	}
	mp := &managedProcess{cmd: cmd, done: make(chan struct{})}
	go func() { mp.err = cmd.Wait(); close(mp.done) }()
	log.Printf("cloudflared started (pid=%d)", cmd.Process.Pid)
	return mp, nil
}

// startQuickTunnel یک Quick Tunnel رایگان (بدون نیاز به اکانت/دامنه) برای یک آدرس
// محلی اجرا می‌کند و هاست‌نیم *.trycloudflare.com اختصاص‌یافته را از لاگ آن استخراج می‌کند.
func startQuickTunnel(path, target string) (*managedProcess, string, error) {
	cmd := exec.Command(path, "tunnel", "--no-autoupdate", "--url", target)
	cmd.Env = os.Environ()
	var logBuf bytes.Buffer
	logMu := &sync.Mutex{}
	writer := &syncedBufferWriter{buf: &logBuf, mu: logMu}
	cmd.Stdout = writer
	cmd.Stderr = writer
	if err := cmd.Start(); err != nil {
		return nil, "", fmt.Errorf("failed to start quick tunnel: %w", err)
	}
	mp := &managedProcess{cmd: cmd, done: make(chan struct{})}
	go func() { mp.err = cmd.Wait(); close(mp.done) }()

	re := regexp.MustCompile(`https://[a-zA-Z0-9-]+\.trycloudflare\.com`)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		logMu.Lock()
		snapshot := logBuf.String()
		logMu.Unlock()
		if m := re.FindString(snapshot); m != "" {
			return mp, m, nil
		}
		select {
		case <-mp.done:
			return nil, "", fmt.Errorf("cloudflared exited before a tunnel URL was assigned")
		case <-time.After(300 * time.Millisecond):
		}
	}
	return mp, "", fmt.Errorf("timed out waiting for a trycloudflare.com URL to be assigned")
}

// syncedBufferWriter یک io.Writer ساده و thread-safe برای گرفتن لاگ‌های cloudflared است.
type syncedBufferWriter struct {
	buf *bytes.Buffer
	mu  *sync.Mutex
}

func (w *syncedBufferWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func stopCloudflared() {
	cfCmdMu.Lock()
	defer cfCmdMu.Unlock()
	if runningCF != nil {
		stopProcessWithTimeout(runningCF, cloudflaredStopTimeout, "cloudflared")
		runningCF = nil
	}
	for _, mp := range cfQuickProcesses {
		stopProcessWithTimeout(mp, cloudflaredStopTimeout, "cloudflared (quick tunnel)")
	}
	cfQuickProcesses = nil
	cfQuickURLsMu.Lock()
	cfQuickURLs = map[string]string{}
	cfQuickURLsMu.Unlock()
}

// stopProcessWithTimeout مثل stopProcess است اما پیام لاگ آن قابل‌تنظیم است (برای
// تمایز sing-box از cloudflared در لاگ‌ها).
func stopProcessWithTimeout(mp *managedProcess, timeout time.Duration, label string) {
	if mp == nil || mp.cmd == nil || mp.cmd.Process == nil {
		return
	}
	_ = mp.cmd.Process.Signal(os.Interrupt)
	select {
	case <-mp.done:
	case <-time.After(timeout):
		log.Printf("%s did not exit within %s, killing it", label, timeout)
		_ = mp.cmd.Process.Kill()
		<-mp.done
	}
}

// startCloudflareTunnel نقطه‌ی ورود اصلی است: بر اساس Mode تنظیم‌شده، یکی از سه
// مسیر (api_token / tunnel_token / quick) را اجرا می‌کند.
func startCloudflareTunnel() error {
	cfCmdMu.Lock()
	defer cfCmdMu.Unlock()

	stopCloudflared_locked()

	state := readStateOrDefault()
	cfg := state.Cloudflare
	if cfg.Mode == "" {
		return fmt.Errorf("Cloudflare tunnel is not configured yet — set it up from the Settings page")
	}

	path, err := findCloudflared()
	if err != nil {
		log.Println("cloudflared not found — attempting automatic download")
		if path, err = downloadCloudflared(); err != nil {
			return fmt.Errorf("cloudflared is not installed and automatic download failed: %w", err)
		}
	}

	switch cfg.Mode {
	case "api_token":
		if cfg.APIToken == "" || cfg.ZoneName == "" {
			return fmt.Errorf("api_token mode requires both an API Token and a domain (zone)")
		}
		zoneID, accountID := cfg.ZoneID, cfg.AccountID
		if zoneID == "" || accountID == "" {
			zoneID, accountID, err = cfResolveZone(cfg.APIToken, cfg.ZoneName)
			if err != nil {
				return err
			}
			cfg.ZoneID, cfg.AccountID = zoneID, accountID
		}
		tunnelID, tunnelToken, err := cfEnsureTunnel(cfg.APIToken, accountID, cfg)
		if err != nil {
			return err
		}
		cfg.TunnelID = tunnelID

		routes := computeTunnelRoutes(state)
		if err := cfPushIngress(cfg.APIToken, accountID, tunnelID, routes); err != nil {
			return fmt.Errorf("failed to push ingress config: %w", err)
		}
		if err := cfUpsertDNS(cfg.APIToken, zoneID, tunnelID, routes); err != nil {
			log.Printf("startCloudflareTunnel: DNS sync had errors (continuing): %v", err)
		}

		// state (zone_id/account_id/tunnel_id تازه‌کشف‌شده) را ذخیره می‌کنیم.
		state.Cloudflare = cfg
		_ = writeState(state)

		mp, err := startCloudflaredWithToken(path, tunnelToken)
		if err != nil {
			return err
		}
		runningCF = mp
		log.Printf("Cloudflare tunnel running — %d public route(s) under %s", len(routes), cfg.ZoneName)

	case "tunnel_token":
		if cfg.TunnelToken == "" {
			return fmt.Errorf("tunnel_token mode requires a Tunnel Token")
		}
		mp, err := startCloudflaredWithToken(path, cfg.TunnelToken)
		if err != nil {
			return err
		}
		runningCF = mp
		log.Println("Cloudflare tunnel running (routes are managed manually in the Cloudflare dashboard)")

	case "quick":
		clashAddr, _ := getClashAPIAddr()
		targets := map[string]string{
			"panel": "http://127.0.0.1" + apiPort,
			"dash":  "http://" + clashAddr,
		}
		newURLs := map[string]string{}
		var procs []*managedProcess
		for label, target := range targets {
			mp, url, err := startQuickTunnel(path, target)
			if err != nil {
				log.Printf("startCloudflareTunnel: quick tunnel for %s failed: %v", label, err)
				continue
			}
			procs = append(procs, mp)
			newURLs[label] = url
		}
		if len(procs) == 0 {
			return fmt.Errorf("failed to start any quick tunnel — check that cloudflared can reach the internet")
		}
		cfQuickProcesses = procs
		cfQuickURLsMu.Lock()
		cfQuickURLs = newURLs
		cfQuickURLsMu.Unlock()
		log.Printf("Quick tunnels running: %v (note: quick tunnels only support HTTP targets — Docker service routes need api_token mode)", newURLs)

	default:
		return fmt.Errorf("unknown Cloudflare tunnel mode %q", cfg.Mode)
	}
	return nil
}

// stopCloudflared_locked نسخه‌ی داخلی stopCloudflared است که فرض می‌کند cfCmdMu از
// قبل قفل شده (برای فراخوانی از داخل startCloudflareTunnel).
func stopCloudflared_locked() {
	if runningCF != nil {
		stopProcessWithTimeout(runningCF, cloudflaredStopTimeout, "cloudflared")
		runningCF = nil
	}
	for _, mp := range cfQuickProcesses {
		stopProcessWithTimeout(mp, cloudflaredStopTimeout, "cloudflared (quick tunnel)")
	}
	cfQuickProcesses = nil
	cfQuickURLsMu.Lock()
	cfQuickURLs = map[string]string{}
	cfQuickURLsMu.Unlock()
}

// syncCloudflareRoutesAsync پس از تغییر DockerServices یا Services، در حالت api_token
// ingress را در پس‌زمینه به‌روزرسانی می‌کند (بدون این‌که درخواست کاربر را بلاک کند).
// در حالت‌های دیگر (یا وقتی تونل اصلاً روشن نیست) کاری انجام نمی‌دهد.
func syncCloudflareRoutesAsync() {
	go func() {
		state := readStateOrDefault()
		if state.Cloudflare.Mode != "api_token" || state.Cloudflare.APIToken == "" || state.Cloudflare.TunnelID == "" {
			return
		}
		routes := computeTunnelRoutes(state)
		if err := cfPushIngress(state.Cloudflare.APIToken, state.Cloudflare.AccountID, state.Cloudflare.TunnelID, routes); err != nil {
			log.Printf("syncCloudflareRoutesAsync: failed to push updated ingress: %v", err)
			return
		}
		if err := cfUpsertDNS(state.Cloudflare.APIToken, state.Cloudflare.ZoneID, state.Cloudflare.TunnelID, routes); err != nil {
			log.Printf("syncCloudflareRoutesAsync: DNS sync had errors: %v", err)
		}
		log.Printf("Cloudflare ingress synced (%d routes) after a service change", len(routes))
	}()
}

// ---------------------------------------------------------------------
// ثبت‌نام حساب WARP
// ---------------------------------------------------------------------
const (
	apiURL  = "https://api.cloudflareclient.com/v0a4005/reg"
	charset = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
)

var warpHTTPClient = &http.Client{Timeout: warpHTTPTimeout}

func randomString(length int) string {
	b := make([]byte, length)
	for i := range b {
		b[i] = charset[randInt(len(charset))]
	}
	return string(b)
}

// randInt یک عدد تصادفی امن و بدون بایاس در بازه‌ی [0, max) برمی‌گرداند.
func randInt(max int) int {
	if max <= 0 {
		return 0
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(max)))
	if err != nil {
		panic(err)
	}
	return int(n.Int64())
}

func generateWireGuardKeypair() (privateKeyB64, publicKeyB64 string, err error) {
	curve := ecdh.X25519()
	privateKey, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("failed to generate X25519 key: %w", err)
	}
	privateKeyB64 = base64.StdEncoding.EncodeToString(privateKey.Bytes())
	publicKeyB64 = base64.StdEncoding.EncodeToString(privateKey.PublicKey().Bytes())
	return privateKeyB64, publicKeyB64, nil
}

type WarpAccount struct {
	PrivateKey    string
	V4            string
	V6            string
	PeerPublicKey string
	Reserved      []byte
}

func RegisterWarpAccount() (*WarpAccount, error) {
	privateKey, publicKey, err := generateWireGuardKeypair()
	if err != nil {
		return nil, err
	}
	installID := randomString(22)
	fcmToken := installID + ":APA91b" + randomString(134)

	payload := map[string]interface{}{
		"key":        publicKey,
		"install_id": installID,
		"fcm_token":  fcmToken,
		"tos":        time.Now().UTC().Format(time.RFC3339),
		"type":       "Android",
		"locale":     "en_US",
	}
	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal JSON: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, apiURL, bytes.NewReader(jsonPayload))
	if err != nil {
		return nil, fmt.Errorf("failed to build request: %w", err)
	}
	// هدرهایی که کلاینت اندروید WARP واقعی می‌فرستد؛ بدون این‌ها API کلادفلر
	// معمولاً درخواست را با 403 رد می‌کند.
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("User-Agent", "okhttp/3.12.1")
	req.Header.Set("CF-Client-Version", "a-6.30")

	resp, err := warpHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	config, ok := result["config"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("missing config object")
	}
	iface, ok := config["interface"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("missing interface object")
	}
	addresses, ok := iface["addresses"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("missing addresses object")
	}
	v4, ok := addresses["v4"].(string)
	if !ok {
		return nil, fmt.Errorf("missing v4 address")
	}
	v6, ok := addresses["v6"].(string)
	if !ok {
		return nil, fmt.Errorf("missing v6 address")
	}
	peers, ok := config["peers"].([]interface{})
	if !ok || len(peers) == 0 {
		return nil, fmt.Errorf("missing peers array")
	}
	peer, ok := peers[0].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid peer entry")
	}
	peerPublicKey, ok := peer["public_key"].(string)
	if !ok {
		return nil, fmt.Errorf("missing peer public_key")
	}
	clientIDB64, ok := config["client_id"].(string)
	if !ok {
		return nil, fmt.Errorf("missing client_id")
	}
	reserved, err := base64.StdEncoding.DecodeString(clientIDB64)
	if err != nil {
		return nil, fmt.Errorf("failed to decode client_id: %w", err)
	}

	return &WarpAccount{
		PrivateKey:    privateKey,
		V4:            v4,
		V6:            v6,
		PeerPublicKey: peerPublicKey,
		Reserved:      reserved,
	}, nil
}

// ---------------------------------------------------------------------
// تولید کانفیگ‌های WireGuard برای اندپوینت‌های WARP
// ---------------------------------------------------------------------
var warpEndpoints = []string{
	"162.159.195.1:4500",
	"162.159.195.1:1701",
	"162.159.192.1:4500",
	"162.159.195.1:2408",
	"162.159.192.1:1701",
	"162.159.193.3:1701",
	"162.159.192.1:500",
	"162.159.193.3:500",
	"162.159.192.1:2408",
	"162.159.193.3:4500",
	"162.159.195.1:500",
	"162.159.193.3:2408",
	"2606:4700:d0::3cd7:73cc:615b:bf06:4500",
	"2606:4700:d0::a29f:c001:500",
	"2606:4700:d0::a29f:c001:1701",
	"2606:4700:d0::a29f:c001:2408",
	"2606:4700:d0::a29f:c001:4500",
}

type AddWarpRequest struct {
	Tag        string `json:"tag"`
	PrivateKey string `json:"private_key"`
	Reserved   []int  `json:"reserved"`
}

const (
	defaultV4            = "172.16.0.2"
	defaultV6            = "2606:4700:110:8ffb:a0e3:e5ca:8b89:c3d8"
	defaultPeerPublicKey = "bmXOC+F1FxEMF9dyiK2H5/1SUtzH0JuVo51h2wPfgyo="
)

type WireGuardConfig struct {
	Type       string   `json:"type"`
	Tag        string   `json:"tag"`
	Address    []string `json:"address"`
	PrivateKey string   `json:"private_key"`
	MTU        int      `json:"mtu"`
	Peers      []Peer   `json:"peers"`
}

type Peer struct {
	Address    string   `json:"address"`
	Port       int      `json:"port"`
	PublicKey  string   `json:"public_key"`
	AllowedIPs []string `json:"allowed_ips"`
	Reserved   []int    `json:"reserved"`
}

func parseEndpoint(endpoint string) (host string, port int, err error) {
	parts := strings.Split(endpoint, ":")
	if len(parts) < 2 {
		return "", 0, fmt.Errorf("invalid endpoint format: %s", endpoint)
	}
	portStr := parts[len(parts)-1]
	port, err = strconv.Atoi(portStr)
	if err != nil {
		return "", 0, fmt.Errorf("invalid port in endpoint %s: %w", endpoint, err)
	}
	host = strings.Join(parts[:len(parts)-1], ":")
	return host, port, nil
}

func GenerateWireGuardConfigs(prefix string, account *WarpAccount, endpoints []string) ([]WireGuardConfig, error) {
	if account == nil {
		return nil, fmt.Errorf("account is nil")
	}
	if len(endpoints) == 0 {
		return nil, nil
	}

	reservedInts := make([]int, len(account.Reserved))
	for i, b := range account.Reserved {
		reservedInts[i] = int(b)
	}
	addresses := []string{
		account.V4 + "/32",
		account.V6 + "/128",
	}

	var configs []WireGuardConfig
	for _, ep := range endpoints {
		host, port, err := parseEndpoint(ep)
		if err != nil {
			return nil, fmt.Errorf("failed to parse endpoint %s: %w", ep, err)
		}
		tag := fmt.Sprintf("%s-%s:%d", prefix, host, port)
		config := WireGuardConfig{
			Type:       "wireguard",
			Tag:        tag,
			Address:    addresses,
			PrivateKey: account.PrivateKey,
			MTU:        1280,
			Peers: []Peer{
				{
					Address:    host,
					Port:       port,
					PublicKey:  account.PeerPublicKey,
					AllowedIPs: []string{"0.0.0.0/0", "::/0"},
					Reserved:   reservedInts,
				},
			},
		}
		configs = append(configs, config)
	}
	return configs, nil
}

// ---------------------------------------------------------------------
// گروه‌بندی نودهای WARP بر اساس تگ (prefix-host:port)
// ---------------------------------------------------------------------
// چون هر تگ به‌شکل "<prefix>-<host>:<port>" ساخته می‌شود و host می‌تواند خودش
// شامل ":" باشد (IPv6)، جداسازی متنیِ prefix از روی یک جداکننده ثابت غیرممکن
// است. راه‌حل قابل‌اعتماد: چون لیست warpEndpoints ثابت و از قبل شناخته‌شده است،
// برای هر تگ، طولانی‌ترین پسوند شناخته‌شده‌ی منطبق را پیدا می‌کنیم؛ باقیمانده
// همان prefix/گروه است. این هم روی تگ‌های تازه و هم روی نمونه‌های قدیمی کار می‌کند.
var warpEndpointSuffixes []string

func init() {
	for _, ep := range warpEndpoints {
		host, port, err := parseEndpoint(ep)
		if err != nil {
			continue
		}
		warpEndpointSuffixes = append(warpEndpointSuffixes, fmt.Sprintf("-%s:%d", host, port))
	}
	sort.Slice(warpEndpointSuffixes, func(i, j int) bool {
		return len(warpEndpointSuffixes[i]) > len(warpEndpointSuffixes[j])
	})
}

// groupPrefixForTag تگ یک اندپوینت WireGuard را به prefix/گروهش می‌شکند.
// اگر تگ با فرمت شناخته‌شده مطابقت نداشت (مثلاً یک نود دستی/سفارشی)، خود تگ
// به‌عنوان یک گروه تک‌عضوی درنظر گرفته می‌شود و grouped برابر false برمی‌گردد.
func groupPrefixForTag(tag string) (prefix string, grouped bool) {
	for _, suf := range warpEndpointSuffixes {
		if len(tag) > len(suf) && strings.HasSuffix(tag, suf) {
			return tag[:len(tag)-len(suf)], true
		}
	}
	return tag, false
}

// ---------------------------------------------------------------------
// رندر / اعتبارسنجی / persist کانفیگ (بازنویسی کامل و اصلاح‌شده)
// ---------------------------------------------------------------------
func asSlice(v interface{}) []interface{} {
	s, _ := v.([]interface{})
	return s
}

func cloneMap(m map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func toInterfaceSlice(ss []string) []interface{} {
	out := make([]interface{}, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func toFloat(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

func findExistingOutbound(tmpl map[string]interface{}, tag string) map[string]interface{} {
	for _, o := range asSlice(tmpl["outbounds"]) {
		if m, ok := o.(map[string]interface{}); ok {
			if t, _ := m["tag"].(string); t == tag {
				return m
			}
		}
	}
	return nil
}

// renderConfig یک config.json کامل از روی template + nodes می‌سازد، بدون این‌که
// نقشه‌ی tmpl ورودی را تغییر دهد (template.json باید همیشه به شکل خام/پایه باقی بماند).
//
// نکات مهم نسبت به نسخه‌ی قبلی:
//  1. رفع باگ اصلی: هر outbound دیگری غیر از "auto" (urltest) و "select-*"
//     (selector) دست‌نخورده می‌ماند.
//  2. هر گروه WARP (prefix) حالا یک نود urltest اختصاصی خودش می‌گیرد
//     ("<prefix>-auto") که فقط بین اندپوینت‌های همان گروه بهترین را انتخاب می‌کند.
//  3. رفع باگ انتخاب پیش‌فرض: هر selector صراحتاً یک فیلد "default" می‌گیرد،
//     پس sing-box دیگر به‌صورت ضمنی اولین آیتم لیست را انتخاب نمی‌کند. اگر
//     یک گروه WARP پیش‌فرض تنظیم شده باشد (state.json)، هدف پیش‌فرض سرویس‌های
//     تازه دقیقاً «نود Auto همان گروه پیش‌فرض» است.
func renderConfig(tmpl map[string]interface{}, nodes []interface{}, state AppState) (map[string]interface{}, error) {
	if tmpl == nil {
		return nil, fmt.Errorf("template is empty")
	}

	cfg := cloneMap(tmpl)

	var wireguardNodes []interface{}
	var otherNodes []interface{}
	var wireguardTags []string
	groupMembers := map[string][]string{}
	var groupOrder []string
	seenGroup := map[string]bool{}

	for _, node := range nodes {
		nMap, ok := node.(map[string]interface{})
		if !ok {
			continue
		}
		nodeType, _ := nMap["type"].(string)
		if nodeType == "wireguard" || nodeType == "tailscale" {
			wireguardNodes = append(wireguardNodes, node)
			tag, _ := nMap["tag"].(string)
			if tag == "" {
				continue
			}
			wireguardTags = append(wireguardTags, tag)
			prefix, grouped := groupPrefixForTag(tag)
			if grouped {
				if !seenGroup[prefix] {
					seenGroup[prefix] = true
					groupOrder = append(groupOrder, prefix)
				}
				groupMembers[prefix] = append(groupMembers[prefix], tag)
			}
		} else {
			otherNodes = append(otherNodes, node)
		}
	}
	sort.Strings(groupOrder)

	cfg["endpoints"] = wireguardNodes

	// تنظیمات پایه برای نودهای urltest (interval/tolerance/url) — از outbound
	// موجود "auto" در template قرض گرفته می‌شود، اگر باشد.
	urltestDefaults := map[string]interface{}{
		"interval":  "10m",
		"tolerance": 50,
		"url":       "http://www.gstatic.com/generate_204",
	}
	if base := findExistingOutbound(tmpl, "auto"); base != nil {
		for _, k := range []string{"interval", "tolerance", "url"} {
			if v, ok := base[k]; ok {
				urltestDefaults[k] = v
			}
		}
	}

	// نود urltest اختصاصی هر گروه — همیشه به‌صورت پویا ساخته می‌شود، هرگز در
	// template.json ذخیره نمی‌شود (درست مثل رفتار قبلی برای "auto" و "select-*").
	var groupAutoOutbounds []interface{}
	var groupAutoTags []string
	for _, prefix := range groupOrder {
		tags := append([]string{}, groupMembers[prefix]...)
		sort.Strings(tags)
		autoTag := prefix + "-auto"
		groupAutoTags = append(groupAutoTags, autoTag)
		node := map[string]interface{}{
			"tag":       autoTag,
			"type":      "urltest",
			"interval":  urltestDefaults["interval"],
			"tolerance": urltestDefaults["tolerance"],
			"url":       urltestDefaults["url"],
			"outbounds": toInterfaceSlice(tags),
		}
		groupAutoOutbounds = append(groupAutoOutbounds, node)
	}

	// "auto" سراسری: بین نودهای auto هر گروه انتخاب می‌کند (سریع‌تر از تست تک‌تک
	// همه‌ی اندپوینت‌های خام)، به‌علاوه‌ی هر اندپوینت گروه‌بندی‌نشده (نودهای دستی).
	var globalAutoMembers []string
	globalAutoMembers = append(globalAutoMembers, groupAutoTags...)
	for _, tag := range wireguardTags {
		if _, grouped := groupPrefixForTag(tag); !grouped {
			globalAutoMembers = append(globalAutoMembers, tag)
		}
	}
	if len(globalAutoMembers) == 0 {
		globalAutoMembers = []string{"direct"}
	}

	// گزینه‌های در دسترس روی هر selector سرویس: auto سراسری + auto هر گروه +
	// خود اندپوینت‌های خام (برای پین‌کردن دستی روی یک IP:PORT خاص در صورت نیاز).
	seenOpt := map[string]bool{}
	var selectorOptions []string
	addOpt := func(t string) {
		if t != "" && !seenOpt[t] {
			seenOpt[t] = true
			selectorOptions = append(selectorOptions, t)
		}
	}
	addOpt("auto")
	for _, t := range groupAutoTags {
		addOpt(t)
	}
	for _, t := range wireguardTags {
		addOpt(t)
	}

	// هدف پیش‌فرضِ fallback برای selectorهایی که هنوز "default" معتبر ندارند:
	// اولویت با نود Auto گروه پیش‌فرض (state.json)، سپس auto سراسری، سپس هر
	// گزینه‌ی دیگری که موجود باشد.
	fallbackDefault := "auto"
	if state.DefaultWarpGroup != "" {
		candidate := state.DefaultWarpGroup + "-auto"
		if seenOpt[candidate] {
			fallbackDefault = candidate
		}
	}
	if !seenOpt[fallbackDefault] {
		switch {
		case len(groupAutoTags) > 0:
			fallbackDefault = groupAutoTags[0]
		case len(wireguardTags) > 0:
			fallbackDefault = wireguardTags[0]
		default:
			fallbackDefault = "direct"
		}
	}

	var newOutbounds []interface{}
	if existing, ok := tmpl["outbounds"].([]interface{}); ok {
		for _, out := range existing {
			outMap, _ := out.(map[string]interface{})
			outType, _ := outMap["type"].(string)
			tag, _ := outMap["tag"].(string)

			switch {
			case outType == "urltest" && tag == "auto":
				clone := cloneMap(outMap)
				clone["outbounds"] = toInterfaceSlice(globalAutoMembers)
				newOutbounds = append(newOutbounds, clone)
			case outType == "selector" && strings.HasPrefix(tag, "select-"):
				clone := cloneMap(outMap)
				clone["outbounds"] = toInterfaceSlice(selectorOptions)
				def, _ := clone["default"].(string)
				if def == "" || !seenOpt[def] {
					clone["default"] = fallbackDefault
				}
				newOutbounds = append(newOutbounds, clone)
			default:
				// هر outbound دیگری (proxy، direct، هرچیز سفارشی) دست‌نخورده می‌ماند
				newOutbounds = append(newOutbounds, out)
			}
		}
	}
	newOutbounds = append(newOutbounds, groupAutoOutbounds...)
	newOutbounds = append(newOutbounds, otherNodes...)
	cfg["outbounds"] = newOutbounds

	return cfg, nil
}

// validateConfig کانفیگ را در یک فایل موقت (نه config.json واقعی) می‌نویسد و با
// «sing-box check» اعتبارسنجی می‌کند تا کانفیگ سالمِ در حال اجرا هرگز با یک
// کانفیگ نامعتبر جایگزین نشود.
func validateConfig(cfg map[string]interface{}) error {
	buf, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	tmpFile, err := os.CreateTemp("", "singbox-check-*.json")
	if err != nil {
		return fmt.Errorf("failed to create temp file for validation: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := tmpFile.Write(buf); err != nil {
		tmpFile.Close()
		return fmt.Errorf("failed to write temp config: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("failed to finalize temp config: %w", err)
	}

	singBoxPath, err := findSingBox()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), singBoxCheckTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, singBoxPath, "check", "-c", tmpPath)
	output, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("sing-box check timed out after %s", singBoxCheckTimeout)
	}
	if err != nil {
		return fmt.Errorf("sing-box validation failed:\n%s", string(output))
	}
	return nil
}

// persist سه فایل را به‌صورت اتمیک می‌نویسد. اگر هرکدام شکست بخورد، خطا برگردانده
// می‌شود؛ چون نوشتن‌ها اتمیک هستند (write+rename)، هرگز فایل نصفه روی دیسک نمی‌ماند.
func persist(tmplRaw, nodesRaw []byte, cfg map[string]interface{}) error {
	if err := atomicWriteFile(templateFile, tmplRaw, 0644); err != nil {
		return fmt.Errorf("failed to persist template.json: %w", err)
	}
	if err := atomicWriteFile(nodesFile, nodesRaw, 0644); err != nil {
		return fmt.Errorf("failed to persist nodes.json: %w", err)
	}
	if err := writeJSONAtomic(configFile, cfg); err != nil {
		return fmt.Errorf("failed to persist config.json: %w", err)
	}
	return nil
}

// applyChangeFromRaw: parse -> render (با state.json فعلی) -> validate -> persist -> restart.
// اگر validate شکست بخورد، هیچ فایلی روی دیسک تغییر نمی‌کند (کانفیگ سالم قبلی محفوظ می‌ماند).
// فراخوان باید mu.Lock() را از قبل گرفته باشد.
func applyChangeFromRaw(tmplRaw, nodesRaw []byte) error {
	var tmpl map[string]interface{}
	if err := json.Unmarshal(tmplRaw, &tmpl); err != nil {
		return fmt.Errorf("invalid template JSON: %w", err)
	}
	var nodes []interface{}
	if err := json.Unmarshal(nodesRaw, &nodes); err != nil {
		return fmt.Errorf("invalid nodes JSON: %w", err)
	}

	state := readStateOrDefault()

	cfg, err := renderConfig(tmpl, nodes, state)
	if err != nil {
		return err
	}
	if err := validateConfig(cfg); err != nil {
		return err
	}
	if err := persist(tmplRaw, nodesRaw, cfg); err != nil {
		return err
	}
	return restartSingBox()
}

// applyChangeFromStruct برای هندلرهایی است که tmpl/nodes را به‌صورت map در حافظه
// تغییر می‌دهند؛ آن‌ها را marshal کرده و از همان مسیر امن applyChangeFromRaw عبور می‌دهد.
func applyChangeFromStruct(tmpl map[string]interface{}, nodes []interface{}) error {
	tmplRaw, err := json.MarshalIndent(tmpl, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal template: %w", err)
	}
	nodesRaw, err := json.MarshalIndent(nodes, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal nodes: %w", err)
	}
	return applyChangeFromRaw(tmplRaw, nodesRaw)
}

// ---------------------------------------------------------------------
// هندلرها
// ---------------------------------------------------------------------
func getConfigsHandler(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	defer mu.RUnlock()

	tmplData, err := os.ReadFile(templateFile)
	if err != nil {
		log.Printf("Error reading template.json: %v", err)
		tmplData = []byte("{}")
	}
	nodesData, err := os.ReadFile(nodesFile)
	if err != nil {
		log.Printf("Error reading nodes.json: %v", err)
		nodesData = []byte("[]")
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"template": string(tmplData),
		"nodes":    string(nodesData),
	})
}

func statusHandler(w http.ResponseWriter, r *http.Request) {
	singBoxCmdMu.Lock()
	mp := runningSingBox
	singBoxCmdMu.Unlock()

	running := false
	pid := 0
	if mp != nil {
		select {
		case <-mp.done:
			running = false
		default:
			running = true
			pid = mp.cmd.Process.Pid
		}
	}
	path, _ := findSingBox()
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"running":       running,
		"pid":           pid,
		"sing_box_path": path,
	})
}

func rebuildHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Template string `json:"template"`
		Nodes    string `json:"nodes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("rebuildHandler: invalid JSON: %v", err)
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Invalid JSON"})
		return
	}
	if strings.TrimSpace(req.Template) == "" || strings.TrimSpace(req.Nodes) == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Both template and nodes must be provided"})
		return
	}

	mu.Lock()
	defer mu.Unlock()

	if err := applyChangeFromRaw([]byte(req.Template), []byte(req.Nodes)); err != nil {
		log.Printf("rebuildHandler: %v", err)
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"message": "Configuration saved, validated, and sing-box restarted successfully!"})
}

// addServiceToTemplate یک سرویس جدید (in-<name> + select-<name> + قانون route)
// را به tmpl اضافه می‌کند. هم توسط addServiceHandler و هم در bootstrap نصب تازه
// استفاده می‌شود تا رفتار کاملاً یکسان باشد.
func addServiceToTemplate(tmpl map[string]interface{}, name string, port int, defaultTarget string) error {
	for _, in := range asSlice(tmpl["inbounds"]) {
		m, ok := in.(map[string]interface{})
		if !ok {
			continue
		}
		if m["tag"] == "in-"+name {
			return fmt.Errorf("service %q already exists", name)
		}
		if pf, ok := toFloat(m["listen_port"]); ok && int(pf) == port {
			return fmt.Errorf("port %d is already used by another service", port)
		}
	}

	tmpl["inbounds"] = append(asSlice(tmpl["inbounds"]), map[string]interface{}{
		"tag":         "in-" + name,
		"listen":      "127.0.0.1",
		"listen_port": port,
		"type":        "mixed",
	})

	sel := map[string]interface{}{
		"tag":       "select-" + name,
		"type":      "selector",
		"outbounds": []interface{}{"auto"},
	}
	if defaultTarget != "" {
		sel["default"] = defaultTarget
	}
	tmpl["outbounds"] = append(asSlice(tmpl["outbounds"]), sel)

	route, _ := tmpl["route"].(map[string]interface{})
	if route == nil {
		route = map[string]interface{}{}
	}
	newRule := map[string]interface{}{
		"action":   "route",
		"inbound":  []interface{}{"in-" + name},
		"outbound": "select-" + name,
	}
	route["rules"] = append([]interface{}{newRule}, asSlice(route["rules"])...)
	tmpl["route"] = route
	return nil
}

func addServiceHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		Port int    `json:"port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("addServiceHandler: invalid JSON: %v", err)
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Invalid JSON"})
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	log.Printf("addServiceHandler: name=%s, port=%d", req.Name, req.Port)

	if !serviceNameRe.MatchString(req.Name) {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Service name must be 1-32 characters: letters, digits, underscore, hyphen"})
		return
	}
	if req.Port < 1 || req.Port > 65535 {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Invalid port (1-65535)"})
		return
	}

	mu.Lock()
	defer mu.Unlock()

	var tmpl map[string]interface{}
	if err := readJSON(templateFile, &tmpl); err != nil {
		log.Printf("addServiceHandler: failed to read template.json: %v", err)
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"error": "Failed to read template.json"})
		return
	}
	var nodes []interface{}
	if err := readJSON(nodesFile, &nodes); err != nil && !os.IsNotExist(err) {
		log.Printf("addServiceHandler: failed to read nodes.json: %v", err)
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"error": "Failed to read nodes.json"})
		return
	}

	state := readStateOrDefault()
	defaultTarget := "auto"
	if state.DefaultWarpGroup != "" {
		defaultTarget = state.DefaultWarpGroup + "-auto"
	}

	if err := addServiceToTemplate(tmpl, req.Name, req.Port, defaultTarget); err != nil {
		jsonResponse(w, http.StatusConflict, map[string]interface{}{"error": err.Error()})
		return
	}

	if err := applyChangeFromStruct(tmpl, nodes); err != nil {
		log.Printf("addServiceHandler: %v", err)
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"message": "Service added, validated, and sing-box restarted successfully!"})
}

func deleteServiceHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("deleteServiceHandler: invalid JSON: %v", err)
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Invalid JSON"})
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	log.Printf("deleteServiceHandler: name=%s", req.Name)

	if req.Name == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Service name is required"})
		return
	}

	mu.Lock()
	defer mu.Unlock()

	var tmpl map[string]interface{}
	if err := readJSON(templateFile, &tmpl); err != nil {
		log.Printf("deleteServiceHandler: failed to read template.json: %v", err)
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"error": "Failed to read template.json"})
		return
	}
	var nodes []interface{}
	if err := readJSON(nodesFile, &nodes); err != nil && !os.IsNotExist(err) {
		log.Printf("deleteServiceHandler: failed to read nodes.json: %v", err)
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"error": "Failed to read nodes.json"})
		return
	}

	found := false
	var newInbounds []interface{}
	for _, in := range asSlice(tmpl["inbounds"]) {
		inMap, _ := in.(map[string]interface{})
		if inMap["tag"] == "in-"+req.Name {
			found = true
			continue
		}
		newInbounds = append(newInbounds, in)
	}
	tmpl["inbounds"] = newInbounds

	var newOutbounds []interface{}
	for _, out := range asSlice(tmpl["outbounds"]) {
		outMap, _ := out.(map[string]interface{})
		if outMap["tag"] == "select-"+req.Name {
			continue
		}
		newOutbounds = append(newOutbounds, out)
	}
	tmpl["outbounds"] = newOutbounds

	if route, ok := tmpl["route"].(map[string]interface{}); ok {
		var newRules []interface{}
		for _, rule := range asSlice(route["rules"]) {
			ruleMap, _ := rule.(map[string]interface{})
			isTarget := false
			for _, tag := range asSlice(ruleMap["inbound"]) {
				if tagStr, ok := tag.(string); ok && tagStr == "in-"+req.Name {
					isTarget = true
					break
				}
			}
			if !isTarget {
				newRules = append(newRules, rule)
			}
		}
		route["rules"] = newRules
		tmpl["route"] = route
	}

	if !found {
		jsonResponse(w, http.StatusNotFound, map[string]interface{}{"error": fmt.Sprintf("Service %q not found", req.Name)})
		return
	}

	if err := applyChangeFromStruct(tmpl, nodes); err != nil {
		log.Printf("deleteServiceHandler: %v", err)
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"message": fmt.Sprintf("Service '%s' deleted successfully!", req.Name)})
}

// editServiceHandler نام و/یا پورت یک سرویس موجود را تغییر می‌دهد: تگ inbound،
// تگ selector مرتبط، و ارجاع‌های route rule را هماهنگ به‌روزرسانی می‌کند.
func editServiceHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OldName string `json:"old_name"`
		NewName string `json:"new_name"`
		NewPort int    `json:"new_port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("editServiceHandler: invalid JSON: %v", err)
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Invalid JSON"})
		return
	}
	req.OldName = strings.TrimSpace(req.OldName)
	req.NewName = strings.TrimSpace(req.NewName)
	log.Printf("editServiceHandler: old_name=%s, new_name=%s, new_port=%d", req.OldName, req.NewName, req.NewPort)

	if req.OldName == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "old_name is required"})
		return
	}
	if !serviceNameRe.MatchString(req.NewName) {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Service name must be 1-32 characters: letters, digits, underscore, hyphen"})
		return
	}
	if req.NewPort < 1 || req.NewPort > 65535 {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Invalid port (1-65535)"})
		return
	}

	mu.Lock()
	defer mu.Unlock()

	var tmpl map[string]interface{}
	if err := readJSON(templateFile, &tmpl); err != nil {
		log.Printf("editServiceHandler: failed to read template.json: %v", err)
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"error": "Failed to read template.json"})
		return
	}
	var nodes []interface{}
	if err := readJSON(nodesFile, &nodes); err != nil && !os.IsNotExist(err) {
		log.Printf("editServiceHandler: failed to read nodes.json: %v", err)
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"error": "Failed to read nodes.json"})
		return
	}

	oldInboundTag := "in-" + req.OldName
	newInboundTag := "in-" + req.NewName
	oldSelectorTag := "select-" + req.OldName
	newSelectorTag := "select-" + req.NewName
	nameChanged := req.NewName != req.OldName

	var targetInbound map[string]interface{}
	for _, in := range asSlice(tmpl["inbounds"]) {
		m, ok := in.(map[string]interface{})
		if !ok {
			continue
		}
		if m["tag"] == oldInboundTag {
			targetInbound = m
			continue
		}
		if nameChanged && m["tag"] == newInboundTag {
			jsonResponse(w, http.StatusConflict, map[string]interface{}{"error": fmt.Sprintf("A service named %q already exists", req.NewName)})
			return
		}
		if pf, ok := toFloat(m["listen_port"]); ok && int(pf) == req.NewPort {
			jsonResponse(w, http.StatusConflict, map[string]interface{}{"error": fmt.Sprintf("Port %d is already used by another service", req.NewPort)})
			return
		}
	}
	if targetInbound == nil {
		jsonResponse(w, http.StatusNotFound, map[string]interface{}{"error": fmt.Sprintf("Service %q not found", req.OldName)})
		return
	}

	targetInbound["tag"] = newInboundTag
	targetInbound["listen_port"] = req.NewPort

	if nameChanged {
		for _, out := range asSlice(tmpl["outbounds"]) {
			if m, ok := out.(map[string]interface{}); ok && m["tag"] == oldSelectorTag {
				m["tag"] = newSelectorTag
			}
		}
		if route, ok := tmpl["route"].(map[string]interface{}); ok {
			for _, rule := range asSlice(route["rules"]) {
				ruleMap, ok := rule.(map[string]interface{})
				if !ok {
					continue
				}
				inboundList := asSlice(ruleMap["inbound"])
				for i, tag := range inboundList {
					if tagStr, ok := tag.(string); ok && tagStr == oldInboundTag {
						inboundList[i] = newInboundTag
					}
				}
				if ob, ok := ruleMap["outbound"].(string); ok && ob == oldSelectorTag {
					ruleMap["outbound"] = newSelectorTag
				}
			}
		}
	}

	if err := applyChangeFromStruct(tmpl, nodes); err != nil {
		log.Printf("editServiceHandler: %v", err)
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"message": fmt.Sprintf("Service %q updated", req.NewName)})
}

// ---------------------------------------------------------------------
// وب‌سرویس‌های جدا (Docker): برای نمایش iframe در پنل + مسیر عمومی تونل Cloudflare.
// اینها مستقل از جدول Services (پروکسی‌های sing-box) هستند و خودشان یک فرانت وب دارند.
// ---------------------------------------------------------------------
var dockerServiceNameRe = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

func dockerServicesHandler(w http.ResponseWriter, r *http.Request) {
	state := readStateOrDefault()
	if state.DockerServices == nil {
		state.DockerServices = []DockerService{}
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"services": state.DockerServices})
}

func addDockerServiceHandler(w http.ResponseWriter, r *http.Request) {
	var req DockerService
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Invalid JSON"})
		return
	}
	req.Name = strings.ToLower(strings.TrimSpace(req.Name))
	if !dockerServiceNameRe.MatchString(req.Name) {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Name must be 1-32 lowercase letters/digits/hyphens (used as a subdomain)"})
		return
	}
	if req.Port < 1 || req.Port > 65535 {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Invalid port (1-65535)"})
		return
	}
	if req.Path == "" {
		req.Path = "/"
	}
	req.PublicURL = strings.TrimSpace(req.PublicURL)

	state := readStateOrDefault()
	for _, s := range state.DockerServices {
		if s.Name == req.Name {
			jsonResponse(w, http.StatusConflict, map[string]interface{}{"error": fmt.Sprintf("A docker service named %q already exists", req.Name)})
			return
		}
	}
	state.DockerServices = append(state.DockerServices, req)
	if err := writeState(state); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"error": "Failed to save: " + err.Error()})
		return
	}
	syncCloudflareRoutesAsync()
	jsonResponse(w, http.StatusOK, map[string]interface{}{"message": fmt.Sprintf("Docker service %q added", req.Name)})
}

func deleteDockerServiceHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Invalid JSON"})
		return
	}
	name := strings.ToLower(strings.TrimSpace(req.Name))

	state := readStateOrDefault()
	kept := state.DockerServices[:0]
	found := false
	for _, s := range state.DockerServices {
		if s.Name == name {
			found = true
			continue
		}
		kept = append(kept, s)
	}
	if !found {
		jsonResponse(w, http.StatusNotFound, map[string]interface{}{"error": fmt.Sprintf("Docker service %q not found", name)})
		return
	}
	state.DockerServices = kept
	if err := writeState(state); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"error": "Failed to save: " + err.Error()})
		return
	}
	syncCloudflareRoutesAsync()
	jsonResponse(w, http.StatusOK, map[string]interface{}{"message": fmt.Sprintf("Docker service %q removed", name)})
}

// settingsHandler وضعیت کلی تنظیمات قابل‌مدیریت از UI را برمی‌گرداند.
func settingsHandler(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"admin_token_set": getAdminToken() != "",
		"bind_addr":       bindAddr,
		"api_port":        apiPort,
	})
}

// updateAdminTokenHandler توکن مدیریتی را از داخل UI تغییر می‌دهد. مقدار خالی
// احراز هویت را غیرفعال می‌کند (فقط برای شبکه‌ی محلی/قابل‌اعتماد).
func updateAdminTokenHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		NewToken string `json:"new_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Invalid JSON"})
		return
	}
	token := strings.TrimSpace(req.NewToken)
	if token != "" && len(token) < 8 {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Token must be at least 8 characters (or empty to disable authentication)"})
		return
	}
	if err := setAdminToken(token); err != nil {
		log.Printf("updateAdminTokenHandler: failed to persist state.json: %v", err)
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"error": "Failed to save token: " + err.Error()})
		return
	}
	if token == "" {
		log.Println("⚠️  ADMIN_TOKEN از UI پاک شد — API مدیریت اکنون بدون احراز هویت است.")
		jsonResponse(w, http.StatusOK, map[string]interface{}{"message": "Admin token cleared — the management API is now unauthenticated. Only do this on a trusted local network."})
		return
	}
	log.Println("🔒 ADMIN_TOKEN از صفحه‌ی Settings به‌روزرسانی شد.")
	jsonResponse(w, http.StatusOK, map[string]interface{}{"message": "Admin token updated"})
}

// singboxInfoHandler اطلاعات نسخه/مسیر باینری sing-box شناسایی‌شده روی سیستم را برمی‌گرداند.
func singboxInfoHandler(w http.ResponseWriter, r *http.Request) {
	path, err := findSingBox()
	resp := map[string]interface{}{
		"found":           err == nil,
		"path":            path,
		"default_version": getSetting("SINGBOX_VERSION", defaultSingBoxVersion),
		"os":              runtime.GOOS,
		"arch":            runtime.GOARCH,
	}
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, verr := exec.CommandContext(ctx, path, "version").CombinedOutput()
		if verr == nil {
			lines := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)
			resp["version"] = strings.TrimSpace(lines[0])
		}
	}
	jsonResponse(w, http.StatusOK, resp)
}

// singboxDownloadHandler دانلود/نصب دستی یک نسخه‌ی مشخص از sing-box را از UI فعال می‌کند.
func singboxDownloadHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Version string `json:"version"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	version := strings.TrimSpace(req.Version)
	if version == "" {
		version = getSetting("SINGBOX_VERSION", defaultSingBoxVersion)
	}
	path, err := downloadSingBox(version)
	if err != nil {
		log.Printf("singboxDownloadHandler: %v", err)
		jsonResponse(w, http.StatusBadGateway, map[string]interface{}{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"message": "sing-box downloaded and installed successfully", "path": path})
}

// ---------------------------------------------------------------------
// تنظیمات مبتنی‌بر env (BIND_ADDR, SINGBOX_PATH, CLOUDFLARED_INSTALL_DIR, ...)
// از صفحه‌ی Settings
// ---------------------------------------------------------------------
func envSettingsHandler(w http.ResponseWriter, r *http.Request) {
	state := readStateOrDefault()
	out := make(map[string]interface{}, len(managedEnvKeys))
	for _, key := range managedEnvKeys {
		effective := getSetting(key, "")
		_, overridden := state.EnvOverrides[key]
		out[key] = map[string]interface{}{
			"value":             effective,
			"overridden_by_ui":  overridden,
			"requires_restart":  envKeysRequiringRestart[key],
			"env_value_present": strings.TrimSpace(os.Getenv(key)) != "",
		}
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"settings": out})
}

func updateEnvSettingsHandler(w http.ResponseWriter, r *http.Request) {
	var req map[string]string
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Invalid JSON"})
		return
	}
	allowed := make(map[string]bool, len(managedEnvKeys))
	for _, k := range managedEnvKeys {
		allowed[k] = true
	}
	var restartNeeded []string
	for key, value := range req {
		if !allowed[key] {
			jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": fmt.Sprintf("%q is not a manageable setting", key)})
			return
		}
		if err := setSetting(key, value); err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"error": "Failed to save: " + err.Error()})
			return
		}
		if envKeysRequiringRestart[key] {
			restartNeeded = append(restartNeeded, key)
		}
	}
	msg := "Settings updated"
	if len(restartNeeded) > 0 {
		msg += fmt.Sprintf(" — restart the manager process for %s to take effect", strings.Join(restartNeeded, ", "))
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"message": msg})
}

// ---------------------------------------------------------------------
// تنظیمات و کنترل تونل Cloudflare از صفحه‌ی Settings
// ---------------------------------------------------------------------
func cloudflareSettingsHandler(w http.ResponseWriter, r *http.Request) {
	state := readStateOrDefault()
	cfg := state.Cloudflare

	cfCmdMu.Lock()
	running := runningCF != nil || len(cfQuickProcesses) > 0
	cfCmdMu.Unlock()

	cfQuickURLsMu.RLock()
	quickURLs := make(map[string]string, len(cfQuickURLs))
	for k, v := range cfQuickURLs {
		quickURLs[k] = v
	}
	cfQuickURLsMu.RUnlock()

	var routes []string
	// serviceHosts نگاشت "کلید منطقی" (panel/dash/اسم هر Docker service) به هاست‌نیم
	// عمومی واقعی است. فرانت‌اند iframe ها را از روی همین می‌سازد، نه با حدس زدن
	// window.location.hostname + پورت محلی (که زیر تونل که هر سرویس زیردامنه‌ی
	// جدای خودش را دارد، اصلاً درست نیست).
	serviceHosts := map[string]string{}
	if cfg.Mode == "api_token" && cfg.ZoneName != "" {
		for _, r := range computeTunnelRoutes(state) {
			routes = append(routes, r.Hostname)
			serviceHosts[r.Key] = r.Hostname
		}
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"mode":                 cfg.Mode,
		"zone_name":            cfg.ZoneName,
		"tunnel_name":          cfg.TunnelName,
		"tunnel_id":            cfg.TunnelID,
		"api_token_set":        cfg.APIToken != "",
		"tunnel_token_set":     cfg.TunnelToken != "",
		"running":              running,
		"quick_tunnel_urls":    quickURLs,
		"routes":               routes,
		"service_hosts":        serviceHosts,
		"dashboard_public_url": cfg.DashboardPublicURL,
	})
}

func updateCloudflareSettingsHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode               string `json:"mode"`
		APIToken           string `json:"api_token"`
		ZoneName           string `json:"zone_name"`
		TunnelToken        string `json:"tunnel_token"`
		TunnelName         string `json:"tunnel_name"`
		DashboardPublicURL string `json:"dashboard_public_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Invalid JSON"})
		return
	}
	req.Mode = strings.TrimSpace(req.Mode)
	if req.Mode != "api_token" && req.Mode != "tunnel_token" && req.Mode != "quick" {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "mode must be one of: api_token, tunnel_token, quick"})
		return
	}
	if req.Mode == "api_token" && (strings.TrimSpace(req.APIToken) == "" || strings.TrimSpace(req.ZoneName) == "") {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "api_token mode requires both api_token and zone_name"})
		return
	}
	if req.Mode == "tunnel_token" && strings.TrimSpace(req.TunnelToken) == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "tunnel_token mode requires tunnel_token"})
		return
	}

	state := readStateOrDefault()
	cfg := state.Cloudflare
	cfg.Mode = req.Mode
	if strings.TrimSpace(req.APIToken) != "" {
		cfg.APIToken = strings.TrimSpace(req.APIToken)
	}
	if strings.TrimSpace(req.ZoneName) != req.ZoneName || req.ZoneName != "" {
		// اگر دامنه تغییر کرده، zone/account/tunnel قبلی دیگر معتبر نیستند و باید دوباره resolve شوند.
		if strings.TrimSpace(req.ZoneName) != cfg.ZoneName {
			cfg.ZoneID, cfg.AccountID, cfg.TunnelID = "", "", ""
		}
		cfg.ZoneName = strings.TrimSpace(req.ZoneName)
	}
	if strings.TrimSpace(req.TunnelToken) != "" {
		cfg.TunnelToken = strings.TrimSpace(req.TunnelToken)
	}
	if strings.TrimSpace(req.TunnelName) != "" {
		cfg.TunnelName = strings.TrimSpace(req.TunnelName)
	}
	// برخلاف توکن‌ها، این فیلد محرمانه نیست، پس همیشه با مقدار فرم جایگزین می‌شود
	// (رشته‌ی خالی یعنی کاربر عمداً override را پاک کرده).
	cfg.DashboardPublicURL = strings.TrimSpace(req.DashboardPublicURL)
	state.Cloudflare = cfg
	if err := writeState(state); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"error": "Failed to save: " + err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"message": "Cloudflare tunnel settings saved"})
}

func cloudflareStartHandler(w http.ResponseWriter, r *http.Request) {
	if err := startCloudflareTunnel(); err != nil {
		log.Printf("cloudflareStartHandler: %v", err)
		jsonResponse(w, http.StatusBadGateway, map[string]interface{}{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"message": "Cloudflare tunnel started"})
}

func cloudflareStopHandler(w http.ResponseWriter, r *http.Request) {
	stopCloudflared()
	jsonResponse(w, http.StatusOK, map[string]interface{}{"message": "Cloudflare tunnel stopped"})
}

func cloudflareRestartHandler(w http.ResponseWriter, r *http.Request) {
	if err := startCloudflareTunnel(); err != nil {
		log.Printf("cloudflareRestartHandler: %v", err)
		jsonResponse(w, http.StatusBadGateway, map[string]interface{}{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"message": "Cloudflare tunnel restarted"})
}

// ---------------------------------------------------------------------
// استارت/استاپ/ری‌استارت دستی سینگ‌باکس (مستقل از rebuild)
// ---------------------------------------------------------------------
func singboxStartHandler(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	defer mu.Unlock()
	var tmpl map[string]interface{}
	var nodes []interface{}
	if err := readJSON(templateFile, &tmpl); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"error": "Failed to read template.json"})
		return
	}
	_ = readJSON(nodesFile, &nodes)
	if err := applyChangeFromStruct(tmpl, nodes); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"message": "sing-box started"})
}

func singboxStopHandler(w http.ResponseWriter, r *http.Request) {
	singBoxCmdMu.Lock()
	stopProcess(runningSingBox)
	runningSingBox = nil
	singBoxCmdMu.Unlock()
	jsonResponse(w, http.StatusOK, map[string]interface{}{"message": "sing-box stopped"})
}

func singboxRestartHandler(w http.ResponseWriter, r *http.Request) {
	singboxStartHandler(w, r)
}

func addWarpHandler(w http.ResponseWriter, r *http.Request) {
	var req AddWarpRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("addWarpHandler: invalid JSON: %v", err)
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Invalid request body"})
		return
	}
	req.Tag = strings.TrimSpace(req.Tag)
	log.Printf("addWarpHandler: tag=%s, private_key_len=%d, reserved=%v", req.Tag, len(req.PrivateKey), req.Reserved)

	if !warpTagRe.MatchString(req.Tag) {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Tag must be 1-40 characters: letters, digits, underscore, hyphen"})
		return
	}
	for _, v := range req.Reserved {
		if v < 0 || v > 255 {
			jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Reserved values must be between 0 and 255"})
			return
		}
	}

	var account *WarpAccount
	var err error

	if req.PrivateKey == "" || len(req.Reserved) == 0 {
		log.Printf("addWarpHandler: registering new WARP account")
		account, err = RegisterWarpAccount()
		if err != nil {
			log.Printf("addWarpHandler: registration failed: %v", err)
			jsonResponse(w, http.StatusBadGateway, map[string]interface{}{"error": "Failed to register new account: " + err.Error()})
			return
		}
	} else {
		reservedBytes := make([]byte, len(req.Reserved))
		for i, v := range req.Reserved {
			reservedBytes[i] = byte(v)
		}
		account = &WarpAccount{
			PrivateKey:    req.PrivateKey,
			V4:            defaultV4,
			V6:            defaultV6,
			PeerPublicKey: defaultPeerPublicKey,
			Reserved:      reservedBytes,
		}
	}

	configs, err := GenerateWireGuardConfigs(req.Tag, account, warpEndpoints)
	if err != nil {
		log.Printf("addWarpHandler: GenerateWireGuardConfigs failed: %v", err)
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Failed to generate configs: " + err.Error()})
		return
	}
	if len(configs) == 0 {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "No configs generated"})
		return
	}

	mu.Lock()
	defer mu.Unlock()

	var tmpl map[string]interface{}
	if err := readJSON(templateFile, &tmpl); err != nil {
		log.Printf("addWarpHandler: failed to read template.json: %v", err)
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"error": "Failed to read template.json"})
		return
	}
	var nodes []interface{}
	if err := readJSON(nodesFile, &nodes); err != nil && !os.IsNotExist(err) {
		log.Printf("addWarpHandler: failed to read nodes file: %v", err)
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"error": "Failed to read nodes file: " + err.Error()})
		return
	}

	existingTags := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		if m, ok := n.(map[string]interface{}); ok {
			if t, ok := m["tag"].(string); ok {
				existingTags[t] = true
			}
		}
	}
	var conflicts []string
	for _, cfg := range configs {
		if existingTags[cfg.Tag] {
			conflicts = append(conflicts, cfg.Tag)
		}
	}
	if len(conflicts) > 0 {
		jsonResponse(w, http.StatusConflict, map[string]interface{}{
			"error": "Tag(s) already exist, choose a different prefix: " + strings.Join(conflicts, ", "),
		})
		return
	}
	// همچنین مطمئن می‌شویم که این پیشوند خودش هم‌نام یک گروه موجود نیست
	// (مثلاً prefix "WARP" وقتی از قبل گروه "WARP" با اندپوینت‌های دیگری وجود دارد).
	for _, g := range buildWarpGroups(nodes, "") {
		if g.Tag == req.Tag {
			jsonResponse(w, http.StatusConflict, map[string]interface{}{
				"error": fmt.Sprintf("A WARP group named %q already exists, choose a different prefix", req.Tag),
			})
			return
		}
	}

	for _, cfg := range configs {
		cfgMap := make(map[string]interface{})
		jsonData, _ := json.Marshal(cfg)
		json.Unmarshal(jsonData, &cfgMap)
		nodes = append(nodes, cfgMap)
	}

	state := readStateOrDefault()
	if state.DefaultWarpGroup == "" {
		state.DefaultWarpGroup = req.Tag
		if err := writeState(state); err != nil {
			log.Printf("addWarpHandler: failed to persist state.json: %v", err)
		}
	}

	if err := applyChangeFromStruct(tmpl, nodes); err != nil {
		log.Printf("addWarpHandler: %v", err)
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": err.Error()})
		return
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"message": fmt.Sprintf("%d WARP node(s) injected, validated, and sing-box restarted successfully!", len(configs)),
	})
}

// ---------------------------------------------------------------------
// مدیریت گروه‌های WARP: مشاهده‌ی گروه‌بندی‌شده، تنظیم پیش‌فرض، ویرایش/حذف
// کل گروه، و حذف تک‌تک اندپوینت‌ها.
// ---------------------------------------------------------------------
type WarpEndpointView struct {
	Tag  string `json:"tag"`
	Host string `json:"host"`
	Port int    `json:"port"`
}

type WarpGroupView struct {
	Tag       string             `json:"tag"`
	IsDefault bool               `json:"is_default"`
	Count     int                `json:"count"`
	AutoTag   string             `json:"auto_tag"`
	Endpoints []WarpEndpointView `json:"endpoints"`
}

func buildWarpGroups(nodes []interface{}, defaultGroup string) []WarpGroupView {
	groups := map[string][]WarpEndpointView{}
	var order []string
	seen := map[string]bool{}

	for _, n := range nodes {
		m, ok := n.(map[string]interface{})
		if !ok {
			continue
		}
		t, _ := m["type"].(string)
		if t != "wireguard" && t != "tailscale" {
			continue
		}
		tag, _ := m["tag"].(string)
		if tag == "" {
			continue
		}
		prefix, _ := groupPrefixForTag(tag)
		host, port := "", 0
		if peers, ok := m["peers"].([]interface{}); ok && len(peers) > 0 {
			if pm, ok := peers[0].(map[string]interface{}); ok {
				host, _ = pm["address"].(string)
				if pf, ok := toFloat(pm["port"]); ok {
					port = int(pf)
				}
			}
		}
		if !seen[prefix] {
			seen[prefix] = true
			order = append(order, prefix)
		}
		groups[prefix] = append(groups[prefix], WarpEndpointView{Tag: tag, Host: host, Port: port})
	}
	sort.Strings(order)

	out := make([]WarpGroupView, 0, len(order))
	for _, p := range order {
		eps := groups[p]
		sort.Slice(eps, func(i, j int) bool { return eps[i].Tag < eps[j].Tag })
		out = append(out, WarpGroupView{
			Tag:       p,
			IsDefault: defaultGroup != "" && p == defaultGroup,
			Count:     len(eps),
			AutoTag:   p + "-auto",
			Endpoints: eps,
		})
	}
	return out
}

func warpGroupsHandler(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	defer mu.RUnlock()

	var nodes []interface{}
	if err := readJSON(nodesFile, &nodes); err != nil && !os.IsNotExist(err) {
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"error": "Failed to read nodes.json"})
		return
	}
	state := readStateOrDefault()
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"groups":        buildWarpGroups(nodes, state.DefaultWarpGroup),
		"default_group": state.DefaultWarpGroup,
	})
}

func setDefaultWarpGroupHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tag string `json:"tag"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Invalid JSON"})
		return
	}
	req.Tag = strings.TrimSpace(req.Tag)

	mu.Lock()
	defer mu.Unlock()

	var nodes []interface{}
	if err := readJSON(nodesFile, &nodes); err != nil && !os.IsNotExist(err) {
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"error": "Failed to read nodes.json"})
		return
	}

	if req.Tag != "" {
		found := false
		for _, g := range buildWarpGroups(nodes, "") {
			if g.Tag == req.Tag {
				found = true
				break
			}
		}
		if !found {
			jsonResponse(w, http.StatusNotFound, map[string]interface{}{"error": fmt.Sprintf("WARP group %q not found", req.Tag)})
			return
		}
	}

	state := readStateOrDefault()
	state.DefaultWarpGroup = req.Tag
	if err := writeState(state); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"error": "Failed to persist state: " + err.Error()})
		return
	}

	var tmpl map[string]interface{}
	if err := readJSON(templateFile, &tmpl); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"error": "Failed to read template.json"})
		return
	}
	if err := applyChangeFromStruct(tmpl, nodes); err != nil {
		log.Printf("setDefaultWarpGroupHandler: %v", err)
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"message": "Default WARP group updated"})
}

func deleteWarpGroupHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tag string `json:"tag"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Invalid JSON"})
		return
	}
	req.Tag = strings.TrimSpace(req.Tag)
	if req.Tag == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Tag is required"})
		return
	}

	mu.Lock()
	defer mu.Unlock()

	var nodes []interface{}
	if err := readJSON(nodesFile, &nodes); err != nil && !os.IsNotExist(err) {
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"error": "Failed to read nodes.json"})
		return
	}

	var kept []interface{}
	removed := 0
	for _, n := range nodes {
		if m, ok := n.(map[string]interface{}); ok {
			tag, _ := m["tag"].(string)
			prefix, _ := groupPrefixForTag(tag)
			if prefix == req.Tag {
				removed++
				continue
			}
		}
		kept = append(kept, n)
	}
	if removed == 0 {
		jsonResponse(w, http.StatusNotFound, map[string]interface{}{"error": fmt.Sprintf("WARP group %q not found", req.Tag)})
		return
	}

	state := readStateOrDefault()
	if state.DefaultWarpGroup == req.Tag {
		remaining := buildWarpGroups(kept, "")
		if len(remaining) > 0 {
			state.DefaultWarpGroup = remaining[0].Tag
		} else {
			state.DefaultWarpGroup = ""
		}
		if err := writeState(state); err != nil {
			log.Printf("deleteWarpGroupHandler: failed to persist state.json: %v", err)
		}
	}

	var tmpl map[string]interface{}
	if err := readJSON(templateFile, &tmpl); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"error": "Failed to read template.json"})
		return
	}
	if err := applyChangeFromStruct(tmpl, kept); err != nil {
		log.Printf("deleteWarpGroupHandler: %v", err)
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"message": fmt.Sprintf("Removed %d node(s) from group %q", removed, req.Tag)})
}

func editWarpGroupHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OldTag string `json:"old_tag"`
		NewTag string `json:"new_tag"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Invalid JSON"})
		return
	}
	req.OldTag = strings.TrimSpace(req.OldTag)
	req.NewTag = strings.TrimSpace(req.NewTag)
	if req.OldTag == "" || req.NewTag == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Both old_tag and new_tag are required"})
		return
	}
	if !warpTagRe.MatchString(req.NewTag) {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "New tag must be 1-40 characters: letters, digits, underscore, hyphen"})
		return
	}

	mu.Lock()
	defer mu.Unlock()

	var nodes []interface{}
	if err := readJSON(nodesFile, &nodes); err != nil && !os.IsNotExist(err) {
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"error": "Failed to read nodes.json"})
		return
	}

	if req.NewTag != req.OldTag {
		for _, g := range buildWarpGroups(nodes, "") {
			if g.Tag == req.NewTag {
				jsonResponse(w, http.StatusConflict, map[string]interface{}{"error": fmt.Sprintf("A WARP group named %q already exists", req.NewTag)})
				return
			}
		}
	}

	changed := 0
	var updated []interface{}
	for _, n := range nodes {
		m, ok := n.(map[string]interface{})
		if ok {
			tag, _ := m["tag"].(string)
			prefix, grouped := groupPrefixForTag(tag)
			if grouped && prefix == req.OldTag {
				suffix := tag[len(prefix):]
				clone := cloneMap(m)
				clone["tag"] = req.NewTag + suffix
				updated = append(updated, clone)
				changed++
				continue
			}
		}
		updated = append(updated, n)
	}
	if changed == 0 {
		jsonResponse(w, http.StatusNotFound, map[string]interface{}{"error": fmt.Sprintf("WARP group %q not found", req.OldTag)})
		return
	}

	state := readStateOrDefault()
	if state.DefaultWarpGroup == req.OldTag {
		state.DefaultWarpGroup = req.NewTag
		if err := writeState(state); err != nil {
			log.Printf("editWarpGroupHandler: failed to persist state.json: %v", err)
		}
	}

	var tmpl map[string]interface{}
	if err := readJSON(templateFile, &tmpl); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"error": "Failed to read template.json"})
		return
	}
	if err := applyChangeFromStruct(tmpl, updated); err != nil {
		log.Printf("editWarpGroupHandler: %v", err)
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"message": fmt.Sprintf("Renamed %d node(s) from %q to %q", changed, req.OldTag, req.NewTag)})
}

func deleteWarpNodeHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tag string `json:"tag"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Invalid JSON"})
		return
	}
	req.Tag = strings.TrimSpace(req.Tag)
	if req.Tag == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Tag is required"})
		return
	}

	mu.Lock()
	defer mu.Unlock()

	var nodes []interface{}
	if err := readJSON(nodesFile, &nodes); err != nil && !os.IsNotExist(err) {
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"error": "Failed to read nodes.json"})
		return
	}

	var kept []interface{}
	found := false
	for _, n := range nodes {
		if m, ok := n.(map[string]interface{}); ok {
			if t, _ := m["tag"].(string); t == req.Tag {
				found = true
				continue
			}
		}
		kept = append(kept, n)
	}
	if !found {
		jsonResponse(w, http.StatusNotFound, map[string]interface{}{"error": fmt.Sprintf("Node %q not found", req.Tag)})
		return
	}

	state := readStateOrDefault()
	if state.DefaultWarpGroup != "" {
		stillExists := false
		for _, g := range buildWarpGroups(kept, "") {
			if g.Tag == state.DefaultWarpGroup {
				stillExists = true
				break
			}
		}
		if !stillExists {
			state.DefaultWarpGroup = ""
			if err := writeState(state); err != nil {
				log.Printf("deleteWarpNodeHandler: failed to persist state.json: %v", err)
			}
		}
	}

	var tmpl map[string]interface{}
	if err := readJSON(templateFile, &tmpl); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"error": "Failed to read template.json"})
		return
	}
	if err := applyChangeFromStruct(tmpl, kept); err != nil {
		log.Printf("deleteWarpNodeHandler: %v", err)
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"message": fmt.Sprintf("Node %q removed", req.Tag)})
}

// ---------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------
func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	// override های ذخیره‌شده از صفحه‌ی Settings را قبل از هر استفاده‌ای از getSetting بارگذاری کن.
	loadEnvOverridesCache()

	// BIND_ADDR/API_PORT فقط از استارت بعدی مدیر اعمال می‌شوند (روی خود socket شنود اثر دارند)،
	// برای همین همینجا و قبل از ساخت http.Server override احتمالی را روی متغیرهای واقعی اعمال می‌کنیم.
	bindAddr = getSetting("BIND_ADDR", bindAddr)
	apiPort = ":" + strings.TrimPrefix(getSetting("API_PORT", strings.TrimPrefix(apiPort, ":")), ":")

	// اگر قبلاً از صفحه‌ی Settings توکنی ذخیره شده باشد، بر متغیر محیطی ADMIN_TOKEN اولویت دارد.
	if persisted := readStateOrDefault().AdminToken; persisted != "" {
		adminToken = persisted
	}

	ensureDefaultFiles()
	autoDownloadSingBoxIfMissing()

	if getAdminToken() == "" {
		log.Println("⚠️  ADMIN_TOKEN تنظیم نشده — API مدیریت بدون احراز هویت است. برای امنیت آن را از صفحه‌ی Settings یا با export ADMIN_TOKEN=... ست کنید.")
	} else {
		log.Println("🔒 API مدیریت با ADMIN_TOKEN محافظت می‌شود.")
	}
	if bindAddr == "0.0.0.0" || bindAddr == "" {
		log.Println("⚠️  BIND_ADDR روی تمام اینترفیس‌ها گوش می‌دهد. برای دسترسی فقط-لوکال از BIND_ADDR=127.0.0.1 استفاده کنید.")
	}

	// اگر قبلاً تونل Cloudflare از UI تنظیم شده، آن را هم بالا می‌آوریم.
	if cfg := readStateOrDefault().Cloudflare; cfg.Mode != "" {
		go func() {
			time.Sleep(2 * time.Second) // صبر کوتاه تا sing-box/clash_api بالا بیاید
			if err := startCloudflareTunnel(); err != nil {
				log.Printf("startup: failed to start Cloudflare tunnel: %v", err)
			}
		}()
	}

	// ساخت اولیه‌ی config.json و اجرای sing-box بر اساس فایل‌های فعلی روی دیسک
	func() {
		mu.Lock()
		defer mu.Unlock()
		tmplRaw, err := os.ReadFile(templateFile)
		if err != nil {
			log.Printf("startup: failed to read template.json: %v", err)
			return
		}
		nodesRaw, err := os.ReadFile(nodesFile)
		if err != nil {
			log.Printf("startup: failed to read nodes.json: %v", err)
			return
		}
		if err := applyChangeFromRaw(tmplRaw, nodesRaw); err != nil {
			log.Printf("startup: initial config build/start failed: %v", err)
			log.Println("Manager API is still available at /api/* so you can fix the configuration.")
		}
	}()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(htmlContent))
	})
	http.HandleFunc("/api/get_configs", requireAuth(requireMethod(http.MethodGet, getConfigsHandler)))
	http.HandleFunc("/api/status", requireAuth(requireMethod(http.MethodGet, statusHandler)))
	http.HandleFunc("/api/rebuild", requireAuth(requireMethod(http.MethodPost, rebuildHandler)))
	http.HandleFunc("/api/add_service", requireAuth(requireMethod(http.MethodPost, addServiceHandler)))
	http.HandleFunc("/api/edit_service", requireAuth(requireMethod(http.MethodPost, editServiceHandler)))
	http.HandleFunc("/api/delete_service", requireAuth(requireMethod(http.MethodPost, deleteServiceHandler)))
	http.HandleFunc("/api/settings", requireAuth(requireMethod(http.MethodGet, settingsHandler)))
	http.HandleFunc("/api/settings/admin_token", requireAuth(requireMethod(http.MethodPost, updateAdminTokenHandler)))
	http.HandleFunc("/api/settings/env", requireAuth(requireMethod(http.MethodGet, envSettingsHandler)))
	http.HandleFunc("/api/settings/env/update", requireAuth(requireMethod(http.MethodPost, updateEnvSettingsHandler)))
	http.HandleFunc("/api/singbox/info", requireAuth(requireMethod(http.MethodGet, singboxInfoHandler)))
	http.HandleFunc("/api/singbox/download", requireAuth(requireMethod(http.MethodPost, singboxDownloadHandler)))
	http.HandleFunc("/api/singbox/start", requireAuth(requireMethod(http.MethodPost, singboxStartHandler)))
	http.HandleFunc("/api/singbox/stop", requireAuth(requireMethod(http.MethodPost, singboxStopHandler)))
	http.HandleFunc("/api/singbox/restart", requireAuth(requireMethod(http.MethodPost, singboxRestartHandler)))
	http.HandleFunc("/api/docker_services", requireAuth(requireMethod(http.MethodGet, dockerServicesHandler)))
	http.HandleFunc("/api/add_docker_service", requireAuth(requireMethod(http.MethodPost, addDockerServiceHandler)))
	http.HandleFunc("/api/delete_docker_service", requireAuth(requireMethod(http.MethodPost, deleteDockerServiceHandler)))
	http.HandleFunc("/api/cloudflare/settings", requireAuth(settingsMethodRouter(cloudflareSettingsHandler, updateCloudflareSettingsHandler)))
	http.HandleFunc("/api/cloudflare/start", requireAuth(requireMethod(http.MethodPost, cloudflareStartHandler)))
	http.HandleFunc("/api/cloudflare/stop", requireAuth(requireMethod(http.MethodPost, cloudflareStopHandler)))
	http.HandleFunc("/api/cloudflare/restart", requireAuth(requireMethod(http.MethodPost, cloudflareRestartHandler)))
	http.HandleFunc("/api/add_warp", requireAuth(requireMethod(http.MethodPost, addWarpHandler)))
	http.HandleFunc("/api/warp_groups", requireAuth(requireMethod(http.MethodGet, warpGroupsHandler)))
	http.HandleFunc("/api/set_default_warp_group", requireAuth(requireMethod(http.MethodPost, setDefaultWarpGroupHandler)))
	http.HandleFunc("/api/delete_warp_group", requireAuth(requireMethod(http.MethodPost, deleteWarpGroupHandler)))
	http.HandleFunc("/api/edit_warp_group", requireAuth(requireMethod(http.MethodPost, editWarpGroupHandler)))
	http.HandleFunc("/api/delete_warp_node", requireAuth(requireMethod(http.MethodPost, deleteWarpNodeHandler)))

	server := &http.Server{Addr: bindAddr + apiPort}

	go func() {
		log.Printf("🚀 Manager running on http://%s%s", bindAddr, apiPort)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server failed: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	<-sigCh

	log.Println("Shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)

	singBoxCmdMu.Lock()
	stopProcess(runningSingBox)
	runningSingBox = nil
	singBoxCmdMu.Unlock()

	log.Println("Goodbye.")
}
