package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	mathrand "math/rand"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
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
	"sync/atomic"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------
// تنظیمات
// ---------------------------------------------------------------------
var (
	templateFile = getEnvDefault("TEMPLATE_FILE", "template.json")
	nodesFile    = getEnvDefault("NODES_FILE", "nodes.json")
	configFile   = getEnvDefault("CONFIG_FILE", "config.json")
	stateFile    = getEnvDefault("STATE_FILE", "state.json") // متادیتای داخلی خود مدیر (نه چیزی که sing-box می‌خواند)

	// subscriptionsDir دایرکتوری‌ای است که در آن، خروجی پارس‌شده‌ی هر subscription
	// (فایل جدا، فقط {"outbounds":[...]}) نگه‌داری می‌شود — هرگز داخل template.json
	// دستی نمی‌رود؛ renderConfig آن‌ها را در زمان ساخت config.json مرج می‌کند
	// (به بخش "Subscriptions" مراجعه کنید).
	subscriptionsDir = getEnvDefault("SUBSCRIPTIONS_DIR", "subscriptions")
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

	// پورت reverse proxy عمومی (پیش‌فرض 80) که DockerServiceها را با پیشوند
	// مسیر (/name/...) و پنل مدیریت را روی "/" سرو می‌کند.
	proxyPort = getEnvDefault("PROXY_PORT", "80")

	// توکن مدیریتی اختیاری. اگر خالی باشد API بدون احراز هویت است (فقط برای dev/local).
	// می‌تواند بعداً از داخل UI (صفحه‌ی Settings) نیز تغییر و در state.json ذخیره شود.
	adminToken   = os.Getenv("ADMIN_TOKEN")
	adminTokenMu sync.RWMutex

	// قفل عملیات فایل/کانفیگ (read-write): نوشتن‌ها Lock می‌گیرند، خواندن‌ها RLock.
	mu sync.RWMutex

	serviceNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
	warpTagRe     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)
)

func init() {
	managedEnvKeys = append(managedEnvKeys, backupEnvKeys...)
}

func getEnvDefault(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// managedEnvKeys لیست کلیدهای مبتنی‌بر env هستند که از صفحه‌ی Settings هم قابل
// مدیریت‌اند (نه فقط با export کردن قبل از اجرا).
var managedEnvKeys = []string{
	"BIND_ADDR", "API_PORT", "PROXY_PORT",
	"SINGBOX_PATH", "SINGBOX_VERSION", "SINGBOX_INSTALL_DIR", "SINGBOX_NO_AUTO_DOWNLOAD",
	"CLOUDFLARED_PATH", "CLOUDFLARED_INSTALL_DIR", "DEFAULT_SERVICES",
	"CLASH_SECRET", "CLASH_API_PORT", "MIXED_PORT", "OMNIROUTE_PORT", "MIMO_PORT", "KIMI_PORT", "DEEPSEEK_PORT", "ZAI_PORT", "GROK2API_PORT", "FLARESOLVERR_PORT",
	"MIMO_PROXY_PORT", "KIMI_PROXY_PORT", "DEEPSEEK_PROXY_PORT", "ZAI_PROXY_PORT", "GROK2API_PROXY_PORT",
}

var managedEnvDefaults = map[string]string{
	"BIND_ADDR":                "127.0.0.1",
	"API_PORT":                 "5000",
	"PROXY_PORT":               "80",
	"SINGBOX_VERSION":          "v1.13.16",
	"SINGBOX_INSTALL_DIR":      "bin",
	"SINGBOX_NO_AUTO_DOWNLOAD": "0",
	"CLOUDFLARED_INSTALL_DIR":  "bin",
	"CLASH_API_PORT":           "9090",
	"MIXED_PORT":               "7890",
	"OMNIROUTE_PORT":           "20128",
	"MIMO_PORT":                "3003",
	"KIMI_PORT":                "3002",
	"DEEPSEEK_PORT":            "3005",
	"ZAI_PORT":                 "3001",
	"GROK2API_PORT":            "3004",
	"FLARESOLVERR_PORT":        "8191",
	"MIMO_PROXY_PORT":          "2003",
	"KIMI_PROXY_PORT":          "2002",
	"DEEPSEEK_PROXY_PORT":      "2005",
	"ZAI_PROXY_PORT":           "2001",
	"GROK2API_PROXY_PORT":      "2004",
}

// backupEnvKeys کلیدهای پیکربندی بکاپ/بازیابی هستند. عمداً از همان مکانیزم
// getSetting/setSetting بقیه‌ی تنظیمات استفاده می‌کنند (نه یک ساختار جدا در
// AppState) چون:
//  1. هم از UI و هم با متغیر محیطی در زمان اجرای کانتینر قابل تنظیم‌اند —
//     این دومی برای رفع مشکل مرغ‌وتخم‌مرغ در بازیابی موقع init لازم است: اگر
//     state.json خودش داخل یکی از مسیرهای هنوز-بازیابی‌نشده باشد، بازهم
//     می‌شود مقصد بکاپ را از قبل با env تنظیم کرد.
//  2. رایگان در پنل عمومی «Advanced (environment) settings» هم نمایش داده می‌شوند.
var backupEnvKeys = []string{
	"BACKUP_PATHS_EXTRA", "BACKUP_PASSPHRASE",
	"BACKUP_HOURLY_ENABLED", "BACKUP_HOURLY_KEEP",
	"BACKUP_DAILY_ENABLED", "BACKUP_DAILY_KEEP",
	"BACKUP_MONTHLY_ENABLED", "BACKUP_MONTHLY_KEEP",
	"BACKUP_S3_ENABLED", "BACKUP_S3_ENDPOINT", "BACKUP_S3_REGION", "BACKUP_S3_BUCKET",
	"BACKUP_S3_ACCESS_KEY", "BACKUP_S3_SECRET_KEY", "BACKUP_S3_PREFIX", "BACKUP_S3_USE_SSL",
	"BACKUP_R2_ENABLED", "BACKUP_R2_BUCKET", "BACKUP_R2_ACCESS_KEY", "BACKUP_R2_SECRET_KEY", "BACKUP_R2_PREFIX",
	"BACKUP_TELEGRAM_ENABLED", "BACKUP_TELEGRAM_BOT_TOKEN", "BACKUP_TELEGRAM_CHAT_ID",
}

// envKeysRequiringRestart کلیدهایی هستند که چون روی socket شنود HTTP یا متغیرهای
// سراسری خوانده‌شده در ابتدای main() اثر می‌گذارند، فقط از ری‌استارت بعدی مدیر اعمال می‌شوند.
var envKeysRequiringRestart = map[string]bool{"BIND_ADDR": true, "API_PORT": true, "PROXY_PORT": true}

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
      <div id="clashSecretWarning" style="display:none; background-color: var(--warning); color: #fff; padding: 12px 16px; border-radius: var(--radius); margin-bottom: 20px; font-weight: 500;">
        ⚠️ هشدار: رمز عبور Clash API (CLASH_SECRET) تعیین نشده است. لطفاً برای امنیت بیشتر از <a href="#settings" onclick="showPage('settings'); return false;" style="color: #fff; text-decoration: underline;">صفحه تنظیمات</a> یک رمز تعیین کنید.
      </div>

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
              <label for="svcListenPort">Listen port (Web) <span class="hint">(required)</span></label>
              <input type="number" id="svcListenPort" placeholder="3000" min="1" max="65535">
            </div>
            <div class="field">
              <label for="svcProxyPort">Proxy port <span class="hint">(optional)</span></label>
              <input type="number" id="svcProxyPort" placeholder="2000" min="1" max="65535">
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
            <thead><tr><th>Service</th><th>Listen Port</th><th>Proxy Port</th><th>Public URL</th><th>Live Outbound</th><th></th></tr></thead>
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
          <h2>Global WARP Endpoint</h2>
          <p class="sub">This endpoint (IP:Port) is applied to ALL WARP nodes. Choose a saved working endpoint or scan for new ones in real time.</p>
          
          <div class="row" style="align-items:flex-end; margin-bottom:15px;">
            <div class="field" style="flex:2;">
              <label>Current Global Endpoint</label>
              <div style="display:flex; gap:8px; align-items:center;">
                <div id="globalWarpEndpoint" style="font-family: var(--font-mono); font-size: 14px; background: var(--bg-alt); padding: 8px 12px; border-radius: var(--radius); border: 1px solid var(--border); color: var(--text); flex:1;">Loading...</div>
                <button class="btn btn-ghost btn-sm" onclick="quickTestCurrentEndpoint()" id="btnQuickTest" title="Test latency of current endpoint">
                  ⚡ Quick Test (تست سریع)
                </button>
                <span id="quickTestResult" style="font-weight:bold; font-size:13px;"></span>
              </div>
            </div>
            <button class="btn btn-danger" onclick="resetWarpEndpoint()">
              Reset Default
            </button>
          </div>

          <div class="row" style="align-items:flex-end; margin-bottom:15px; border-top: 1px solid var(--border); padding-top: 15px;">
            <div class="field" style="flex:2;">
              <label for="savedWarpDropdown">Saved Working Endpoints (تغییر سریع بدون اسکن مجدد)</label>
              <select id="savedWarpDropdown" class="input" style="font-family: var(--font-mono);"></select>
            </div>
            <button class="btn btn-primary" onclick="applyDropdownWarpEndpoint()">
              Apply Selected Saved
            </button>
          </div>

          <div class="row" style="align-items:flex-end; border-top: 1px solid var(--border); padding-top: 15px;">
            <div class="field">
              <label for="warpIpType">IP Protocol</label>
              <select id="warpIpType" class="input">
                <option value="both" selected>Both (IPv4 &amp; IPv6)</option>
                <option value="ipv4">IPv4 Only</option>
                <option value="ipv6">IPv6 Only</option>
              </select>
            </div>
            <div class="field">
              <label for="warpPauseTarget">Auto-Pause Target</label>
              <input type="number" id="warpPauseTarget" class="input" value="10" min="1" placeholder="e.g. 10">
            </div>
            <button class="btn btn-primary" onclick="startWarpScan()" id="startScanBtn">
              Start WARP Scan
            </button>
          </div>

          <div id="warpScanResultsContainer" style="display:none; margin-top:20px; border-top: 1px solid var(--border); padding-top: 15px;">
            <div style="display:flex; justify-content:space-between; align-items:center; margin-bottom:12px; background:var(--bg-alt); padding:10px 14px; border-radius:var(--radius); border:1px solid var(--border);">
              <div>
                <div style="font-weight:600; font-size:14px;"><span id="scanStatusTitle">Scanning in progress...</span> (<span id="scanValidCount" style="color:var(--success);">0</span> working / <span id="scanRejectedCount" style="color:var(--danger);">0</span> rejected / <span id="scanTestedCount">0</span> tested)</div>
                <div class="hint" id="scanStatusSub" style="margin-top:2px;">Searching candidate pool...</div>
              </div>
              <div style="display:flex; gap:8px; align-items:center;">
                <button class="btn btn-primary btn-sm" id="btnResumeScan" style="display:none;" onclick="resumeWarpScan()">
                  Continue Scan (ادامه)
                </button>
                <button class="btn btn-ghost btn-sm" id="btnStopScan" onclick="stopWarpScan()">
                  Pause/Stop
                </button>
                <button class="btn btn-primary btn-sm" onclick="applySelectedWarpEndpoint()">
                  Apply Selected
                </button>
              </div>
            </div>

            <div style="max-height: 350px; overflow-y: auto; border: 1px solid var(--border); border-radius: var(--radius); margin-bottom: 15px;">
              <table class="data" style="margin:0;">
                <thead>
                  <tr>
                    <th style="width:50px; text-align:center;">Select</th>
                    <th>Endpoint Address</th>
                    <th>Ping (Latency)</th>
                    <th>Status / Source</th>
                  </tr>
                </thead>
                <tbody id="warpScanResultsBody"></tbody>
              </table>
            </div>

            <button class="btn btn-primary" onclick="applySelectedWarpEndpoint()">
              Apply Selected Endpoint
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
              <label for="singboxVersion">Version to install (one-off)</label>
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
          
          <hr style="margin: 20px 0; border: none; border-top: 1px solid var(--border);">
          <h3 style="margin-bottom: 12px; font-size: 14px;">sing-box Configuration</h3>
          <div class="row" style="margin-bottom:14px;">
            <div class="field" style="flex:1;">
              <label for="env_SINGBOX_PATH">sing-box binary path override</label>
              <input type="text" id="env_SINGBOX_PATH" placeholder="(optional / not set)">
            </div>
            <div class="field" style="flex:1;">
              <label for="env_SINGBOX_VERSION">sing-box default version</label>
              <input type="text" id="env_SINGBOX_VERSION" placeholder="(default: v1.13.16)">
            </div>
          </div>
          <div class="row" style="margin-bottom:14px; align-items: flex-end;">
            <div class="field" style="flex:1;">
              <label for="env_SINGBOX_INSTALL_DIR">sing-box install directory</label>
              <input type="text" id="env_SINGBOX_INSTALL_DIR" placeholder="(default: bin)">
            </div>
            <div class="field" style="flex:1;">
              <label style="display:flex; align-items:center; cursor:pointer;">
                <input type="checkbox" id="env_SINGBOX_NO_AUTO_DOWNLOAD_checkbox" onchange="document.getElementById('env_SINGBOX_NO_AUTO_DOWNLOAD').value = this.checked ? '1' : '0'" style="width:auto; margin-right:8px; margin-bottom:0;">
                Disable sing-box auto-download
              </label>
              <input type="hidden" id="env_SINGBOX_NO_AUTO_DOWNLOAD" value="0">
            </div>
          </div>
          <div class="row">
            <button class="btn btn-primary btn-sm" onclick="saveSingboxEnvSettings()">Save Configuration</button>
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
              <p class="hint" style="margin-top:6px;">Point a single ingress rule for your hostname at <span class="mono">http://127.0.0.1:<span id="cfTunnelTokenProxyPort">80</span></span> — the built-in reverse proxy fans it out to the panel ("/") and every Docker web app ("/&lt;name&gt;/"). No per-service rule needed.</p>
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

          <hr style="margin: 20px 0; border: none; border-top: 1px solid var(--border);">
          <h3 style="margin-bottom: 12px; font-size: 14px;">cloudflared Configuration</h3>
          <div class="row" style="margin-bottom:14px;">
            <div class="field" style="flex:1;">
              <label for="env_CLOUDFLARED_PATH">cloudflared binary path override</label>
              <input type="text" id="env_CLOUDFLARED_PATH" placeholder="(optional / not set)">
            </div>
            <div class="field" style="flex:1;">
              <label for="env_CLOUDFLARED_INSTALL_DIR">cloudflared install directory</label>
              <input type="text" id="env_CLOUDFLARED_INSTALL_DIR" placeholder="(default: bin)">
            </div>
          </div>
          <div class="row">
            <button class="btn btn-primary btn-sm" onclick="saveCloudflaredEnvSettings()">Save Configuration</button>
          </div>
        </div>

        <div class="panel">
          <div class="panel-head"><h2>Subscriptions</h2></div>
          <p class="sub">Import proxy nodes from a remote subscription URL or pasted content — sing-box JSON, Clash JSON/YAML, vmess/vless/trojan/ss/hysteria2 URI lines, plain <span class="mono">IP:PORT</span> lines, or base64-wrapped versions of any of these. Each subscription gets its own selectable group (<span class="mono">&lt;name&gt;-auto</span>) on every service — nodes are cached in their own file, never inlined into the hand-edited template.</p>
          <div class="row" style="align-items:flex-end;">
            <div class="field">
              <label for="subName">Name</label>
              <input type="text" id="subName" placeholder="e.g. myprovider">
            </div>
            <div class="field">
              <label for="subSourceType">Source</label>
              <select id="subSourceType" onchange="toggleSubscriptionSourceFields()">
                <option value="remote">Remote URL</option>
                <option value="local">Paste content</option>
              </select>
            </div>
          </div>
          <div class="field" id="subUrlField" style="margin-top:8px;">
            <label for="subUrl">Subscription URL</label>
            <input type="text" id="subUrl" placeholder="https://provider.example.com/sub?token=...">
          </div>
          <div class="field" id="subContentField" style="margin-top:8px;display:none;">
            <label for="subContent">Subscription content</label>
            <textarea id="subContent" rows="6" placeholder="Paste sing-box JSON, Clash YAML, vmess://... lines, etc."></textarea>
          </div>
          <div class="row" style="margin-top:8px;">
            <button class="btn btn-primary" onclick="addSubscription()">Add &amp; fetch</button>
          </div>
          <table class="data" style="margin-top:14px;">
            <thead><tr><th>Name</th><th>Source</th><th>Nodes</th><th>Last fetched</th><th>Status</th><th></th></tr></thead>
            <tbody id="subscriptionsBody"><tr class="empty-row"><td colspan="6">No subscriptions added yet.</td></tr></tbody>
          </table>
        </div>

        <div class="panel">
          <div class="panel-head"><h2>Backup &amp; Restore</h2></div>
          <p class="sub">/data and /app/data are always included. Restore is checked automatically on container startup if those two are empty and a previous backup exists. <span class="hint">Everything here is also settable via the matching BACKUP_* environment variable at container start — useful for bootstrapping before any restore has happened.</span></p>

          <h3 style="margin:10px 0 6px;font-size:14px;">Paths</h3>
          <div class="row" style="align-items:flex-end;">
            <div class="field"><label>Always included</label><input type="text" value="/data, /app/data" disabled></div>
            <div class="field"><label for="bkExtraPaths">Extra paths/files <span class="hint">(comma-separated, optional)</span></label><input type="text" id="bkExtraPaths" placeholder="/etc/myapp,/root/secrets.txt"></div>
          </div>

          <h3 style="margin:14px 0 6px;font-size:14px;">Encryption</h3>
          <div class="row" style="align-items:flex-end;">
            <div class="field"><label for="bkPassphrase">Passphrase <span class="hint" id="bkPassphraseHint">(empty = archives are not encrypted)</span></label><input type="password" id="bkPassphrase" placeholder="Leave empty to keep unencrypted" autocomplete="new-password"></div>
          </div>

          <h3 style="margin:14px 0 6px;font-size:14px;">Schedule</h3>
          <div class="row" style="align-items:flex-end;">
            <div class="field"><label><input type="checkbox" id="bkHourlyEnabled" onchange="toggleBackupVisibility()"> Hourly</label><input type="number" id="bkHourlyKeep" placeholder="keep (24)" min="1"></div>
            <div class="field"><label><input type="checkbox" id="bkDailyEnabled" onchange="toggleBackupVisibility()"> Daily</label><input type="number" id="bkDailyKeep" placeholder="keep (7)" min="1"></div>
            <div class="field"><label><input type="checkbox" id="bkMonthlyEnabled" onchange="toggleBackupVisibility()"> Monthly</label><input type="number" id="bkMonthlyKeep" placeholder="keep (6)" min="1"></div>
          </div>

          <h3 style="margin:14px 0 6px;font-size:14px;">S3-compatible</h3>
          <div class="row" style="align-items:flex-end;">
            <div class="field"><label><input type="checkbox" id="bkS3Enabled" onchange="toggleBackupVisibility()"> Enabled</label></div>
            <div class="field"><label for="bkS3Endpoint">Endpoint</label><input type="text" id="bkS3Endpoint" placeholder="s3.amazonaws.com"></div>
            <div class="field"><label for="bkS3Region">Region</label><input type="text" id="bkS3Region" placeholder="us-east-1"></div>
            <div class="field"><label for="bkS3Bucket">Bucket</label><input type="text" id="bkS3Bucket"></div>
          </div>
          <div class="row" style="align-items:flex-end;margin-top:8px;">
            <div class="field"><label for="bkS3AccessKey">Access key</label><input type="text" id="bkS3AccessKey" autocomplete="off"></div>
            <div class="field"><label for="bkS3SecretKey">Secret key</label><input type="password" id="bkS3SecretKey" placeholder="Leave empty to keep the current key" autocomplete="new-password"></div>
            <div class="field"><label for="bkS3Prefix">Prefix</label><input type="text" id="bkS3Prefix" placeholder="optional/path"></div>
            <div class="field"><label><input type="checkbox" id="bkS3UseSSL" checked> Use SSL</label></div>
          </div>

          <h3 style="margin:14px 0 6px;font-size:14px;">Cloudflare R2</h3>
          <p class="sub" id="bkR2HelpText"><span class="hint">Reuses the Cloudflare API Token from the Tunnel section to create the bucket, but R2 object access needs its own S3 API credentials — generate those separately under R2 → Manage API tokens and paste them below.</span></p>
          <div class="row" style="align-items:flex-end;">
            <div class="field"><label><input type="checkbox" id="bkR2Enabled" onchange="toggleBackupVisibility()"> Enabled</label></div>
            <div class="field"><label for="bkR2Bucket">Bucket</label><input type="text" id="bkR2Bucket"></div>
            <button class="btn btn-ghost btn-sm" id="btnCreateR2" onclick="createR2Bucket()">Create bucket in Cloudflare</button>
          </div>
          <div class="row" style="align-items:flex-end;margin-top:8px;">
            <div class="field"><label for="bkR2AccessKey">R2 access key</label><input type="text" id="bkR2AccessKey" autocomplete="off"></div>
            <div class="field"><label for="bkR2SecretKey">R2 secret key</label><input type="password" id="bkR2SecretKey" placeholder="Leave empty to keep the current key" autocomplete="new-password"></div>
            <div class="field"><label for="bkR2Prefix">Prefix</label><input type="text" id="bkR2Prefix" placeholder="optional/path"></div>
          </div>

          <h3 style="margin:14px 0 6px;font-size:14px;">Telegram bot</h3>
          <div class="row" style="align-items:flex-end;">
            <div class="field"><label><input type="checkbox" id="bkTgEnabled" onchange="toggleBackupVisibility()"> Enabled</label></div>
            <div class="field"><label for="bkTgBotToken">Bot token</label><input type="password" id="bkTgBotToken" placeholder="Leave empty to keep the current token" autocomplete="new-password"></div>
            <div class="field"><label for="bkTgChatId">Chat ID</label><input type="text" id="bkTgChatId" placeholder="e.g. -1001234567890"></div>
          </div>
          <p class="sub" id="bkTgHelpText"><span class="hint">Archives over ~18MB are automatically split into numbered parts (Telegram's bot download cap is 20MB) and reassembled on restore.</span></p>

          <div class="row" style="margin-top:14px;">
            <button class="btn btn-primary" onclick="saveBackupSettings()">Save settings</button>
            <button class="btn btn-ghost btn-sm" onclick="runBackupNow()">Backup now</button>
          </div>

          <h3 style="margin:16px 0 6px;font-size:14px;">Available backups</h3>
          <table class="data">
            <thead><tr><th>When</th><th>Cadence</th><th>Destination</th><th>Size</th><th>Enc.</th><th></th></tr></thead>
            <tbody id="backupListBody"><tr class="empty-row"><td colspan="6">No backups yet.</td></tr></tbody>
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
    var hash = window.location.hash.substring(1);
    var validPages = ['services', 'warp', 'dashboard', 'raw', 'settings'];
    if (validPages.indexOf(hash) !== -1) {
      showPage(hash);
    } else {
      showPage('services');
    }
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
    if (name === 'settings'){ loadSettings(); loadSingboxInfo(); loadCloudflareSettings(); loadSubscriptions(); loadEnvSettings(); loadBackupSettings(); }
    if (name === 'dashboard') loadDashboardFrame();
    if (window.location.hash !== '#' + name) {
      history.replaceState(null, null, '#' + name);
    }
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
    var services = (typeof lastServices !== 'undefined') ? lastServices.length : 0;
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
      
      var secretWarning = document.getElementById('clashSecretWarning');
      if (secretWarning) {
        var hasSecret = false;
        try {
          if (lastTemplate.experimental && lastTemplate.experimental.clash_api && lastTemplate.experimental.clash_api.secret) {
            hasSecret = true;
          }
        } catch (e) {}
        secretWarning.style.display = hasSecret ? 'none' : 'block';
      }

      lastServices = data.services || [];
      renderServices(lastServices);
      await loadWarpGroups();
      await loadWarpEndpoint();
      loadSelectors();
    } catch (err){
      showMessage('Failed to load configuration: ' + err.message, 'danger');
    }
  }

  var lastServices = [];

  async function renderServices(services){
    var body = document.getElementById('servicesBody');
    document.getElementById('countServices').textContent = services.length;
    if (services.length === 0){
      body.innerHTML = '<tr class="empty-row"><td colspan="6">No services yet — add one above.</td></tr>';
      return;
    }
    var base = await resolvePublicBaseUrl();
    body.innerHTML = '';
    services.forEach(function(svc){
      var name = svc.name;
      var listenPort = svc.listen_port;
      var proxyPort = svc.proxy_port || '-';
      var tr = document.createElement('tr');
      tr.dataset.service = name;

      var tdName = document.createElement('td'); tdName.textContent = name;
      var tdListen = document.createElement('td'); tdListen.className = 'mono'; tdListen.textContent = listenPort;
      var tdProxy = document.createElement('td'); tdProxy.className = 'mono'; tdProxy.textContent = proxyPort;
      
      var url = base + '/' + name + '/';
      var tdUrl = document.createElement('td'); tdUrl.className = 'mono';
      if (listenPort > 0) {
        var link = document.createElement('a');
        link.href = url; link.textContent = '/' + name + '/'; link.target = '_blank'; link.rel = 'noreferrer';
        tdUrl.appendChild(link);
      } else {
        tdUrl.textContent = '-';
      }

      // Live outbound only makes sense if there's a proxy port
      var tdLive = document.createElement('td');
      if (svc.proxy_port > 0) {
        tdLive.className = 'live-outbound';
        tdLive.dataset.service = name;
        tdLive.innerHTML = '<span class="hint">' + (controllerBase ? 'loading…' : 'connect to switch live') + '</span>';
      } else {
        tdLive.textContent = '-';
      }

      var tdAct = document.createElement('td');
      tdAct.style.whiteSpace = 'nowrap';

      var editBtn = document.createElement('button');
      editBtn.className = 'btn btn-ghost btn-sm';
      editBtn.textContent = 'Edit';
      editBtn.onclick = function(){ startEditService(tr, name, listenPort, svc.proxy_port || 0); };

      var delBtn = document.createElement('button');
      delBtn.className = 'btn btn-danger btn-sm';
      delBtn.textContent = 'Delete';
      delBtn.style.marginInlineStart = '6px';
      delBtn.onclick = function(){
        askConfirm('Delete service', 'This removes "' + name + '" and its routing rules/public URL.', function(){
          request('/api/delete_service', { name: name }, 'Service deleted');
        });
      };
      tdAct.appendChild(editBtn);
      tdAct.appendChild(delBtn);

      tr.appendChild(tdName); tr.appendChild(tdListen); tr.appendChild(tdProxy); tr.appendChild(tdUrl); tr.appendChild(tdLive); tr.appendChild(tdAct);
      body.appendChild(tr);
    });
    // Trigger loadSelectors again since the DOM for .live-outbound changed
    if (controllerBase) loadSelectors();
  }

  // Swaps the Name/Port cells for inputs and the action buttons for Save/Cancel,
  // without disturbing the rest of the table.
  function startEditService(tr, name, listenPort, proxyPort){
    var tdName = tr.children[0];
    var tdListen = tr.children[1];
    var tdProxy = tr.children[2];
    var tdAct = tr.children[5];

    tdName.innerHTML = '';
    var nameInput = document.createElement('input');
    nameInput.type = 'text';
    nameInput.value = name;
    nameInput.maxLength = 32;
    nameInput.style.width = '100px';
    tdName.appendChild(nameInput);

    tdListen.innerHTML = '';
    var listenInput = document.createElement('input');
    listenInput.type = 'number';
    listenInput.value = listenPort;
    listenInput.min = 1;
    listenInput.max = 65535;
    listenInput.style.width = '80px';
    tdListen.appendChild(listenInput);

    tdProxy.innerHTML = '';
    var proxyInput = document.createElement('input');
    proxyInput.type = 'number';
    proxyInput.value = proxyPort > 0 ? proxyPort : '';
    proxyInput.min = 1;
    proxyInput.max = 65535;
    proxyInput.style.width = '80px';
    tdProxy.appendChild(proxyInput);

    tdAct.innerHTML = '';
    var saveBtn = document.createElement('button');
    saveBtn.className = 'btn btn-primary btn-sm';
    saveBtn.textContent = 'Save';
    saveBtn.onclick = function(){
      var newName = nameInput.value.trim();
      var newListen = parseInt(listenInput.value, 10);
      var newProxy = parseInt(proxyInput.value, 10);
      if (isNaN(newProxy)) newProxy = 0;

      if (!newName){ showMessage('Service name is required', 'danger'); return; }
      if (isNaN(newListen) || newListen < 1 || newListen > 65535){ showMessage('A valid listen port is required', 'danger'); return; }
      
      saveBtn.disabled = true;
      request('/api/edit_service', { 
        old_name: name, 
        new_name: newName, 
        new_listen_port: newListen,
        new_proxy_port: newProxy
      }, 'Service updated').then(function(ok){
        if (!ok) saveBtn.disabled = false;
      });
    };

    var cancelBtn = document.createElement('button');
    cancelBtn.className = 'btn btn-ghost btn-sm';
    cancelBtn.textContent = 'Cancel';
    cancelBtn.style.marginInlineStart = '6px';
    cancelBtn.onclick = function(){ renderServices(lastServices); };

    tdAct.appendChild(saveBtn);
    tdAct.appendChild(cancelBtn);

    nameInput.focus();
    nameInput.select();
  }

  window.addService = function(){
    var name = document.getElementById('svcName').value.trim();
    var listenPort = parseInt(document.getElementById('svcListenPort').value, 10);
    var proxyPort = parseInt(document.getElementById('svcProxyPort').value, 10);
    if (isNaN(proxyPort)) proxyPort = 0;

    if (!name){ showMessage('Service name is required', 'danger'); return; }
    if (isNaN(listenPort) || listenPort < 1 || listenPort > 65535){ showMessage('A valid listen port is required', 'danger'); return; }
    
    request('/api/add_service', { 
      name: name, 
      listen_port: listenPort,
      proxy_port: proxyPort 
    }, 'Service added').then(function(ok){
      if (ok){
        document.getElementById('svcName').value = '';
        document.getElementById('svcListenPort').value = '';
        document.getElementById('svcProxyPort').value = '';
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

  async function loadWarpEndpoint() {
    try {
      var res = await fetch('/api/warp/endpoint');
      var data = await res.json();
      var el = document.getElementById('globalWarpEndpoint');
      if (el) el.textContent = data.endpoint || 'Not set';
      loadWarpBackupsDropdown();
    } catch (e) {
      console.error('Failed to load warp endpoint:', e);
    }
  }

  var knownWarpDelays = {};

  async function loadWarpBackupsDropdown(){
    try {
      var res = await fetch('/api/warp/endpoint/backups');
      var data = await res.json();
      var dropdown = document.getElementById('savedWarpDropdown');
      if (!dropdown) return;
      dropdown.innerHTML = '';
      var list = data.backups || [];
      var cur = data.current || '';

      list.forEach(function(ep){
        var opt = document.createElement('option');
        opt.value = ep;
        opt.textContent = ep + (knownWarpDelays[ep] ? ' - ' + knownWarpDelays[ep] + 'ms' : '') + (ep === cur ? ' (Current Active)' : (ep === 'engage.cloudflareclient.com:2408' ? ' (Default)' : ''));
        if (ep === cur) opt.selected = true;
        dropdown.appendChild(opt);
      });
      
      // Background fetch delays if not known
      var missing = list.filter(function(ep) { return !knownWarpDelays[ep]; });
      if (missing.length > 0 && typeof isScanning !== 'undefined' && !isScanning) {
        api('/api/warp/endpoint/test_backups', {}).then(function(testData) {
          var updated = false;
          (testData.results || []).forEach(function(resItem) {
            if (resItem.delay > 0) {
              knownWarpDelays[resItem.endpoint] = resItem.delay;
              updated = true;
            }
          });
          if (updated) {
            // Re-render dropdown with new delays
            Array.from(dropdown.options).forEach(function(opt) {
              var ep = opt.value;
              if (knownWarpDelays[ep]) {
                opt.textContent = ep + ' - ' + knownWarpDelays[ep] + 'ms' + (ep === cur ? ' (Current Active)' : (ep === 'engage.cloudflareclient.com:2408' ? ' (Default)' : ''));
              }
            });
          }
        }).catch(function(){}); // Ignore errors in background ping
      }
    } catch(e) {
      console.error('Failed to load warp backups:', e);
    }
  }

  window.quickTestCurrentEndpoint = function(){
    var btn = document.getElementById('btnQuickTest');
    var resultEl = document.getElementById('quickTestResult');
    if (btn) btn.disabled = true;
    if (resultEl) resultEl.innerHTML = '<span class="hint">Testing...</span>';

    api('/api/warp/endpoint/ping_current', {}).then(function(data){
      if (btn) btn.disabled = false;
      if (data.delay > 0) {
        if (data.endpoint) knownWarpDelays[data.endpoint] = data.delay; // Cache for dropdown
        var color = data.delay < 250 ? 'var(--success)' : (data.delay < 450 ? 'orange' : 'var(--danger)');
        if (resultEl) resultEl.innerHTML = '<span style="color:' + color + ';">⚡ ' + data.delay + ' ms</span>';
        if (typeof loadWarpBackupsDropdown === 'function') loadWarpBackupsDropdown();
      } else {
        if (resultEl) resultEl.innerHTML = '<span style="color:var(--text-dim);">Timeout / Failed</span>';
      }
    }).catch(function(err){
      if (btn) btn.disabled = false;
      if (resultEl) resultEl.innerHTML = '<span style="color:var(--danger);">' + err.message + '</span>';
    });
  };

  window.applyDropdownWarpEndpoint = function(){
    var dropdown = document.getElementById('savedWarpDropdown');
    if (!dropdown || !dropdown.value) {
      showMessage('No saved endpoint selected', 'danger');
      return;
    }
    var ep = dropdown.value;
    api('/api/warp/endpoint/apply', { endpoint: ep }).then(function(data){
      showMessage(data.message, 'success');
      loadWarpEndpoint();
      loadWarpGroups();
    }).catch(function(err){
      showMessage(err.message, 'danger');
    });
  };

  var isScanning = false;
  var isPaused = false;
  var validScanResults = [];
  var testedEndpointMap = {};
  var totalTestedCount = 0;
  var totalRejectedCount = 0;
  var targetPauseThreshold = 10;
  var selectedIpType = 'both';
  var scanSessionId = 0;

  window.startWarpScan = function(){
    isScanning = true;
    isPaused = false;
    validScanResults = [];
    testedEndpointMap = {};
    totalTestedCount = 0;
    totalRejectedCount = 0;
    scanSessionId++;
    var currentSession = scanSessionId;
    
    var pauseSelect = document.getElementById('warpPauseTarget');
    targetPauseThreshold = pauseSelect ? parseInt(pauseSelect.value, 10) : 10;
    if (isNaN(targetPauseThreshold) || targetPauseThreshold < 1) targetPauseThreshold = 10;

    var ipTypeSelect = document.getElementById('warpIpType');
    selectedIpType = ipTypeSelect ? ipTypeSelect.value : 'both';

    var container = document.getElementById('warpScanResultsContainer');
    if (container) container.style.display = 'block';

    var btn = document.getElementById('startScanBtn');
    if (btn) btn.disabled = true;

    updateScanUIStatus('Scanning in progress...', 'scanning');
    runScanLoop(currentSession);
  };

  window.resumeWarpScan = function(){
    if (!isScanning && validScanResults.length === 0) return;
    isScanning = true;
    isPaused = false;

    var pauseSelect = document.getElementById('warpPauseTarget');
    var increment = pauseSelect ? parseInt(pauseSelect.value, 10) : 10;
    if (isNaN(increment) || increment < 1) increment = 10;
    targetPauseThreshold = validScanResults.length + increment;

    updateScanUIStatus('Resuming scan...', 'scanning');
    scanSessionId++;
    runScanLoop(scanSessionId);
  };

  window.stopWarpScan = function(){
    isScanning = false;
    isPaused = false;
    updateScanUIStatus('Scan paused/stopped', 'stopped');
    var btn = document.getElementById('startScanBtn');
    if (btn) btn.disabled = false;
  };

  function updateScanUIStatus(subText, state){
    var validCountEl = document.getElementById('scanValidCount');
    var rejectedCountEl = document.getElementById('scanRejectedCount');
    var testedCountEl = document.getElementById('scanTestedCount');
    var subEl = document.getElementById('scanStatusSub');
    var btnResume = document.getElementById('btnResumeScan');
    var btnStop = document.getElementById('btnStopScan');

    if (validCountEl) validCountEl.textContent = validScanResults.length;
    if (rejectedCountEl) rejectedCountEl.textContent = totalRejectedCount;
    if (testedCountEl) testedCountEl.textContent = totalTestedCount;
    if (subEl) subEl.textContent = subText;

    if (state === 'scanning') {
      if (btnResume) btnResume.style.display = 'none';
      if (btnStop) btnStop.style.display = 'inline-block';
    } else if (state === 'paused') {
      if (btnResume) btnResume.style.display = 'inline-block';
      if (btnStop) btnStop.style.display = 'inline-block';
    } else {
      if (btnResume) btnResume.style.display = 'inline-block';
      if (btnStop) btnStop.style.display = 'none';
    }
  }

  async function runScanLoop(sessionId){
    while (isScanning && !isPaused && scanSessionId === sessionId) {
      var excludedList = Object.keys(testedEndpointMap);
      try {
        var res = await api('/api/warp/endpoint/scan_batch', { batch_size: 25, excluded: excludedList, ip_type: selectedIpType });
        if (scanSessionId !== sessionId) break; // Check again after await
        
        var batchTested = res.tested_count || 0;
        totalTestedCount += batchTested;

        var newValid = res.valid_results || [];
        var newValidCount = 0;
        
        newValid.forEach(function(item){
          if (!testedEndpointMap[item.endpoint]) {
            testedEndpointMap[item.endpoint] = true;
            if (item.delay > 0) { // STRICT FILTER: ONLY VALID PING (>0 ms)
              validScanResults.push(item);
              knownWarpDelays[item.endpoint] = item.delay; // Cache
              newValidCount++;
            }
          }
        });
        
        totalRejectedCount += (batchTested - newValidCount);

        // Re-sort valid results by delay ascending
        validScanResults.sort(function(a, b){ return a.delay - b.delay; });
        renderValidScanTable(validScanResults);
        loadWarpBackupsDropdown();

        // Check if we hit pause threshold
        if (validScanResults.length >= targetPauseThreshold) {
          isPaused = true;
          updateScanUIStatus('Paused (Found ' + validScanResults.length + ' working endpoints). Pick an endpoint or click "Continue Scan (ادامه)".', 'paused');
          break;
        }

        if (res.tested_count === 0) {
          isScanning = false;
          updateScanUIStatus('Finished candidate pool search.', 'stopped');
          var btn = document.getElementById('startScanBtn');
          if (btn) btn.disabled = false;
          break;
        }
      } catch (err) {
        console.error('Scan batch failed:', err);
        isPaused = true;
        updateScanUIStatus('Batch error: ' + err.message, 'paused');
        var btn = document.getElementById('startScanBtn');
        if (btn) btn.disabled = false;
        break;
      }
    }
  }

  function renderValidScanTable(results){
    var body = document.getElementById('warpScanResultsBody');
    if (!body) return;

    var currentlySelected = document.querySelector('input[name="selectedWarpEndpoint"]:checked');
    var selectedVal = currentlySelected ? currentlySelected.value : (results.length ? results[0].endpoint : '');

    body.innerHTML = '';

    results.forEach(function(item, idx){
      var tr = document.createElement('tr');
      
      var tdRadio = document.createElement('td');
      tdRadio.style.textAlign = 'center';
      var radio = document.createElement('input');
      radio.type = 'radio';
      radio.name = 'selectedWarpEndpoint';
      radio.value = item.endpoint;
      if (item.endpoint === selectedVal || (!selectedVal && idx === 0)) {
        radio.checked = true;
      }
      tdRadio.appendChild(radio);

      var tdEp = document.createElement('td');
      tdEp.style.fontFamily = 'var(--font-mono)';
      tdEp.style.fontWeight = '600';
      tdEp.textContent = item.endpoint;

      var tdPing = document.createElement('td');
      var color = item.delay < 250 ? 'var(--success)' : (item.delay < 450 ? 'orange' : 'var(--danger)');
      tdPing.innerHTML = '<span style="color:' + color + '; font-weight:bold;">' + item.delay + ' ms</span>';

      var tdType = document.createElement('td');
      if (item.is_default) {
        tdType.innerHTML = '<span class="badge" style="background:var(--primary); color:#fff;">Default Reference</span>';
      } else if (item.is_backup) {
        tdType.innerHTML = '<span class="badge" style="background:#8a2be2; color:#fff;">Saved Backup</span>';
      } else {
        tdType.innerHTML = '<span class="hint">Discovered Candidate</span>';
      }

      tr.appendChild(tdRadio);
      tr.appendChild(tdEp);
      tr.appendChild(tdPing);
      tr.appendChild(tdType);

      body.appendChild(tr);
    });
  }

  window.applySelectedWarpEndpoint = function(){
    var selected = document.querySelector('input[name="selectedWarpEndpoint"]:checked');
    if (!selected) {
      showMessage('Please select a working endpoint from the list', 'danger');
      return;
    }
    var ep = selected.value;
    api('/api/warp/endpoint/apply', { endpoint: ep }).then(function(data){
      showMessage(data.message, 'success');
      loadWarpEndpoint();
      loadWarpGroups();
    }).catch(function(err){
      showMessage(err.message, 'danger');
    });
  };

  window.resetWarpEndpoint = function(){
    askConfirm('Reset Endpoint', 'Reset to engage.cloudflareclient.com:2408?', function(){
      request('/api/warp/endpoint/reset', {}, 'Endpoint reset').then(function(ok){
        if (ok) {
          loadWarpEndpoint();
          loadWarpGroups();
          var container = document.getElementById('warpScanResultsContainer');
          if (container) container.style.display = 'none';
        }
      });
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
      var data = await getSettingsInfo();
      var box = document.getElementById('adminTokenStatus');
      box.innerHTML = '';
      box.appendChild(statCard('Status', data.admin_token_set ? 'Protected' : 'Not set', data.admin_token_set ? 'var(--success)' : 'var(--danger)'));
      var portEl = document.getElementById('cfTunnelTokenProxyPort');
      if (portEl && data.proxy_port) portEl.textContent = data.proxy_port;
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
        settingsPromise = null;
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
      cfInfoPromise = Promise.resolve(data); // همین پاسخ را برای resolvePublicBaseUrl/loadDashboardFrame هم کش کن

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
  // getCloudflareInfo/loadDashboardFrame یک نمونه از پاسخ /api/cloudflare/settings
  // را کش می‌کنند تا هر بار که Settings/Dashboard باز می‌شود دوباره fetch نزنند؛
  // هر جا mode/دامنه/سرویس‌ها ممکن است عوض شده باشد (ذخیره‌ی تنظیمات Cloudflare،
  // افزودن/حذف Docker service) کش را invalidate می‌کنیم.
  // -----------------------------------------------------------------
  var cfInfoPromise = null;
  function getCloudflareInfo(){
    if (!cfInfoPromise){
      cfInfoPromise = fetch('/api/cloudflare/settings').then(function(r){ return r.json(); }).catch(function(){ return {}; });
    }
    return cfInfoPromise;
  }

  var settingsPromise = null;
  function getSettingsInfo(){
    if (!settingsPromise){
      settingsPromise = fetch('/api/settings').then(function(r){ 
        if (!r.ok) throw new Error('API error');
        return r.json(); 
      }).catch(function(err){ 
        console.error('Failed to load settings:', err);
        settingsPromise = null; 
        return {}; 
      });
    }
    return settingsPromise;
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

  // resolvePublicBaseUrl آدرس پایه‌ی عمومی فعلی (بدون "/" انتهایی) را برمی‌گرداند
  // — همان چیزی که reverse proxy محلی روی پورت proxyPort با آن در دسترس است.
  // در حالت api_token، پنل روی یک ساب‌دامین جدا (panel.<domain>) است در حالی که
  // خودِ DockerServiceها روی ریشه‌ی دامنه (service_hosts.apps) سرو می‌شوند —
  // پس آن‌جا باید صریحاً از service_hosts.apps استفاده کرد. در بقیه‌ی حالت‌ها
  // (quick / tunnel_token / بدون تونل) پنل و سرویس‌ها روی همان یک origin هستند،
  // ولی اگر کاربر مستقیماً روی پورت مدیریت (apiPort، مثلاً 5000) باشد، پورت به
  // proxyPort (مثلاً 80) تغییر داده می‌شود تا لینک به سرور reverse proxy اشاره کند.
  async function resolvePublicBaseUrl(){
    var cf = await getCloudflareInfo();
    if (cf.mode === 'api_token' && cf.service_hosts && cf.service_hosts.apps){
      return 'https://' + cf.service_hosts.apps;
    }
    var st = await getSettingsInfo();
    var pPort = (st && st.proxy_port) ? String(st.proxy_port) : '80';
    var aPort = (st && st.api_port) ? String(st.api_port).replace(/^:/, '') : '5000';
    var locPort = window.location.port || (window.location.protocol === 'https:' ? '443' : '80');

    if (locPort === aPort){
      var proto = window.location.protocol;
      var host = window.location.hostname;
      if ((pPort === '80' && proto === 'http:') || (pPort === '443' && proto === 'https:')){
        return proto + '//' + host;
      }
      return proto + '//' + host + ':' + pPort;
    }
    return window.location.origin;
  }

  // -----------------------------------------------------------------
  // -----------------------------------------------------------------
  // Subscriptions: import proxy nodes from a remote URL or pasted content.
  // Refresh is manual-only (no background scheduler) — the person clicks
  // "Add & fetch" or the per-row Refresh button.
  // -----------------------------------------------------------------
  window.toggleSubscriptionSourceFields = function(){
    var isRemote = document.getElementById('subSourceType').value === 'remote';
    document.getElementById('subUrlField').style.display = isRemote ? '' : 'none';
    document.getElementById('subContentField').style.display = isRemote ? 'none' : '';
  };

  function subscriptionStatusText(s){
    if (s.last_error) return s.last_error;
    if (!s.last_fetched) return 'Not fetched yet';
    return 'OK';
  }

  async function loadSubscriptions(){
    try {
      var res = await fetch('/api/subscriptions');
      var data = await res.json();
      var body = document.getElementById('subscriptionsBody');
      if (!body) return;
      var subs = data.subscriptions || [];
      if (!subs.length){
        body.innerHTML = '<tr class="empty-row"><td colspan="6">No subscriptions added yet.</td></tr>';
        return;
      }
      body.innerHTML = '';
      subs.forEach(function(s){
        var tr = document.createElement('tr');
        var tdName = document.createElement('td'); tdName.textContent = s.name + (s.enabled ? '' : ' (disabled)');
        var tdSrc = document.createElement('td'); tdSrc.textContent = s.source_type;
        var tdCount = document.createElement('td'); tdCount.textContent = s.node_count || 0;
        var tdFetched = document.createElement('td'); tdFetched.textContent = s.last_fetched ? new Date(s.last_fetched).toLocaleString() : '—';
        var tdStatus = document.createElement('td'); tdStatus.textContent = subscriptionStatusText(s);
        if (s.last_error) tdStatus.style.color = 'var(--danger)';
        var tdAct = document.createElement('td');
        var refreshBtn = document.createElement('button');
        refreshBtn.className = 'btn btn-ghost btn-sm';
        refreshBtn.textContent = 'Refresh';
        refreshBtn.onclick = function(){
          refreshBtn.disabled = true;
          api('/api/refresh_subscription', { name: s.name }).then(function(data){
            showMessage(data.message || 'Refreshed', 'success');
            loadSubscriptions();
          }).catch(function(err){
            showMessage(err.message, 'danger');
            loadSubscriptions();
          }).finally(function(){ refreshBtn.disabled = false; });
        };
        var toggleBtn = document.createElement('button');
        toggleBtn.className = 'btn btn-ghost btn-sm';
        toggleBtn.textContent = s.enabled ? 'Disable' : 'Enable';
        toggleBtn.onclick = function(){
          api('/api/edit_subscription', { name: s.name, enabled: !s.enabled }).then(function(data){
            showMessage(data.message || 'Updated', 'success');
            loadSubscriptions();
          }).catch(function(err){ showMessage(err.message, 'danger'); });
        };
        var delBtn = document.createElement('button');
        delBtn.className = 'btn btn-danger btn-sm';
        delBtn.textContent = 'Delete';
        delBtn.onclick = function(){
          askConfirm('Remove ' + s.name + '?', 'This removes its ' + (s.node_count || 0) + ' cached node(s) and its group from every service.', function(){
            api('/api/delete_subscription', { name: s.name }).then(function(data){
              showMessage(data.message || 'Removed', 'success');
              loadSubscriptions();
            }).catch(function(err){ showMessage(err.message, 'danger'); });
          });
        };
        tdAct.appendChild(refreshBtn); tdAct.appendChild(toggleBtn); tdAct.appendChild(delBtn);
        tr.appendChild(tdName); tr.appendChild(tdSrc); tr.appendChild(tdCount); tr.appendChild(tdFetched); tr.appendChild(tdStatus); tr.appendChild(tdAct);
        body.appendChild(tr);
      });
    } catch (err){
      showMessage('Failed to load subscriptions: ' + err.message, 'danger');
    }
  }
  window.loadSubscriptions = loadSubscriptions;

  window.addSubscription = function(){
    var name = document.getElementById('subName').value.trim();
    var sourceType = document.getElementById('subSourceType').value;
    var url = document.getElementById('subUrl').value.trim();
    var content = document.getElementById('subContent').value;
    if (!name){ showMessage('Name is required', 'danger'); return; }
    if (sourceType === 'remote' && !url){ showMessage('Subscription URL is required', 'danger'); return; }
    if (sourceType === 'local' && !content.trim()){ showMessage('Subscription content is required', 'danger'); return; }
    api('/api/add_subscription', { name: name, source_type: sourceType, url: url, content: content }).then(function(data){
      showMessage(data.message || 'Added', 'success');
      document.getElementById('subName').value = '';
      document.getElementById('subUrl').value = '';
      document.getElementById('subContent').value = '';
      loadSubscriptions();
    }).catch(function(err){
      showMessage(err.message, 'danger');
    });
  };

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
    PROXY_PORT: 'Public reverse proxy port (needs manager restart)',
    DEFAULT_SERVICES: 'Default proxy services (name:port, e.g. telegram:2083)',
    CLASH_SECRET: 'Clash API Secret (password)',
    CLASH_API_PORT: 'Clash / sing-box API port',
    MIXED_PORT: 'Mixed proxy port',
    OMNIROUTE_PORT: 'OmniRoute port',
    MIMO_PORT: 'MimoApi port',
    KIMI_PORT: 'KimiApi port',
    DEEPSEEK_PORT: 'DeepSeekApi port',
    ZAI_PORT: 'ZaiApi / GlmApi port',
    GROK2API_PORT: 'Grok2Api port',
    FLARESOLVERR_PORT: 'FlareSolverr port'
  };
  async function loadEnvSettings(){
    try {
      var res = await fetch('/api/settings/env');
      var data = await res.json();
      var box = document.getElementById('envSettingsBody');
      if (!box) return;
      box.innerHTML = '';
      
      var settings = data.settings || {};
      var keys = Object.keys(envSettingLabels);
      Object.keys(settings).forEach(function(k){
        if (keys.indexOf(k) === -1) {
          keys.push(k);
        }
      });

      keys.forEach(function(key){
        var s = settings[key] || {};
        var val = s.value || s.default_value || '';
        
        var extInput = document.getElementById('env_' + key);
        if (extInput && extInput.closest('#envSettingsBody') === null) {
            extInput.value = val;
            if (s.overridden_by_ui) extInput.placeholder = '(overridden: ' + s.value + ')';
            else if (s.env_value_present) extInput.placeholder = '(env: ' + s.value + ')';
            else if (s.default_value) extInput.placeholder = '(default: ' + s.default_value + ')';
        }
        
        if (key.startsWith('BACKUP_') || key.startsWith('SINGBOX_') || key.startsWith('CLOUDFLARED_')) {
            return;
        }

        var row = document.createElement('div');
        row.className = 'field';
        row.style.marginBottom = '10px';
        var label = document.createElement('label');
        label.textContent = envSettingLabels[key] || key;
        var input = document.createElement('input');
        input.type = 'text';
        input.id = 'env_' + key;
        input.value = val;

        if (s.overridden_by_ui) {
          input.placeholder = '(overridden in UI: ' + s.value + ')';
        } else if (s.env_value_present) {
          input.placeholder = '(environment variable: ' + s.value + ')';
        } else if (s.default_value) {
          input.placeholder = '(default: ' + s.default_value + ')';
        } else {
          input.placeholder = '(optional / not set)';
        }

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
      settingsPromise = null;
      loadEnvSettings();
    }).catch(function(err){
      showMessage(err.message, 'danger');
    });
  }

  window.saveSingboxEnvSettings = function(){
    var body = {
      SINGBOX_PATH: document.getElementById('env_SINGBOX_PATH').value,
      SINGBOX_VERSION: document.getElementById('env_SINGBOX_VERSION').value,
      SINGBOX_INSTALL_DIR: document.getElementById('env_SINGBOX_INSTALL_DIR').value,
      SINGBOX_NO_AUTO_DOWNLOAD: document.getElementById('env_SINGBOX_NO_AUTO_DOWNLOAD').value
    };
    api('/api/settings/env/update', body).then(function(data){
      showMessage('sing-box configuration saved', 'success');
      settingsPromise = null;
      loadEnvSettings();
    }).catch(function(err){
      showMessage(err.message, 'danger');
    });
  };

  window.saveCloudflaredEnvSettings = function(){
    var body = {
      CLOUDFLARED_PATH: document.getElementById('env_CLOUDFLARED_PATH').value,
      CLOUDFLARED_INSTALL_DIR: document.getElementById('env_CLOUDFLARED_INSTALL_DIR').value
    };
    api('/api/settings/env/update', body).then(function(data){
      showMessage('Cloudflared configuration saved', 'success');
      settingsPromise = null;
      loadEnvSettings();
    }).catch(function(err){
      showMessage(err.message, 'danger');
    });
  };;

  // -----------------------------------------------------------------
  // Backup & Restore
  // -----------------------------------------------------------------
  function backupFmtSize(bytes){
    if (!bytes) return '-';
    var units = ['B','KB','MB','GB'], i = 0, n = bytes;
    while (n >= 1024 && i < units.length - 1){ n /= 1024; i++; }
    return n.toFixed(i === 0 ? 0 : 1) + ' ' + units[i];
  }

  async function loadBackupSettings(){
    try {
      var res = await fetch('/api/backup/settings');
      var data = await res.json();

      document.getElementById('bkExtraPaths').value = (data.paths_extra || []).join(',');
      document.getElementById('bkPassphraseHint').textContent = data.passphrase_set ? '(set — leave empty to keep it, type a new one to change it)' : '(empty = archives are not encrypted)';

      var c = data.cadences || {};
      ['hourly','daily','monthly'].forEach(function(name){
        var cc = c[name] || {};
        document.getElementById('bk' + name.charAt(0).toUpperCase() + name.slice(1) + 'Enabled').checked = !!cc.enabled;
        document.getElementById('bk' + name.charAt(0).toUpperCase() + name.slice(1) + 'Keep').value = cc.keep || '';
      });

      var s3 = data.s3 || {};
      document.getElementById('bkS3Enabled').checked = !!s3.enabled;
      document.getElementById('bkS3Endpoint').value = s3.endpoint || '';
      document.getElementById('bkS3Region').value = s3.region || '';
      document.getElementById('bkS3Bucket').value = s3.bucket || '';
      document.getElementById('bkS3Prefix').value = s3.prefix || '';
      document.getElementById('bkS3UseSSL').checked = s3.use_ssl !== false;
      document.getElementById('bkS3AccessKey').value = s3.access_key_set ? '(set)' : '';

      var r2 = data.cloudflare_r2 || {};
      document.getElementById('bkR2Enabled').checked = !!r2.enabled;
      document.getElementById('bkR2Bucket').value = r2.bucket || '';
      document.getElementById('bkR2Prefix').value = r2.prefix || '';
      document.getElementById('bkR2AccessKey').value = r2.access_key_set ? '(set)' : '';

      var tg = data.telegram || {};
      document.getElementById('bkTgEnabled').checked = !!tg.enabled;
      document.getElementById('bkTgChatId').value = tg.chat_id || '';

      var body = document.getElementById('backupListBody');
      var backups = data.backups || [];
      if (!backups.length){
        body.innerHTML = '<tr class="empty-row"><td colspan="6">No backups yet.</td></tr>';
      } else {
        body.innerHTML = '';
        backups.forEach(function(b){
          var tr = document.createElement('tr');
          var when = new Date(b.timestamp).toLocaleString();
          [when, b.cadence, b.destination, backupFmtSize(b.size_bytes), b.encrypted ? 'yes' : 'no'].forEach(function(v){
            var td = document.createElement('td'); td.textContent = v; tr.appendChild(td);
          });
          var tdAct = document.createElement('td');
          var btn = document.createElement('button');
          btn.className = 'btn btn-danger btn-sm';
          btn.textContent = 'Restore';
          btn.onclick = function(){
            askConfirm('Restore this backup?', 'This overwrites current files in /data, /app/data, and any extra configured paths with the contents of this archive (' + when + ', ' + b.destination + ').', function(){
              api('/api/backup/restore', { destination: b.destination, key: b.key, timestamp: b.timestamp }).then(function(data){
                showMessage(data.message || 'Restored', 'success');
              }).catch(function(err){ showMessage(err.message, 'danger'); });
            });
          };
          tdAct.appendChild(btn);
          tr.appendChild(tdAct);
          body.appendChild(tr);
        });
      }
      toggleBackupVisibility();
    } catch (err){
      showMessage('Failed to load backup settings: ' + err.message, 'danger');
    }
  }
  window.loadBackupSettings = loadBackupSettings;

  window.toggleBackupVisibility = function() {
    var toggle = function(checkboxId, fieldIds) {
      var isChecked = document.getElementById(checkboxId).checked;
      fieldIds.forEach(function(id) {
        var el = document.getElementById(id);
        if (el) {
          var parent = el.closest('.field');
          if (parent) {
            parent.style.display = isChecked ? '' : 'none';
          } else {
            el.style.display = isChecked ? '' : 'none';
          }
        }
      });
    };

    // Schedule
    document.getElementById('bkHourlyKeep').style.display = document.getElementById('bkHourlyEnabled').checked ? '' : 'none';
    document.getElementById('bkDailyKeep').style.display = document.getElementById('bkDailyEnabled').checked ? '' : 'none';
    document.getElementById('bkMonthlyKeep').style.display = document.getElementById('bkMonthlyEnabled').checked ? '' : 'none';

    // S3
    toggle('bkS3Enabled', ['bkS3Endpoint', 'bkS3Region', 'bkS3Bucket', 'bkS3AccessKey', 'bkS3SecretKey', 'bkS3Prefix', 'bkS3UseSSL']);
    // R2
    toggle('bkR2Enabled', ['bkR2Bucket', 'bkR2AccessKey', 'bkR2SecretKey', 'bkR2Prefix', 'btnCreateR2', 'bkR2HelpText']); 
    // Telegram
    toggle('bkTgEnabled', ['bkTgBotToken', 'bkTgChatId', 'bkTgHelpText']);
  };

  window.saveBackupSettings = function(){
    var body = {
      BACKUP_PATHS_EXTRA: document.getElementById('bkExtraPaths').value.trim(),
      BACKUP_HOURLY_ENABLED: document.getElementById('bkHourlyEnabled').checked ? 'true' : 'false',
      BACKUP_HOURLY_KEEP: document.getElementById('bkHourlyKeep').value.trim(),
      BACKUP_DAILY_ENABLED: document.getElementById('bkDailyEnabled').checked ? 'true' : 'false',
      BACKUP_DAILY_KEEP: document.getElementById('bkDailyKeep').value.trim(),
      BACKUP_MONTHLY_ENABLED: document.getElementById('bkMonthlyEnabled').checked ? 'true' : 'false',
      BACKUP_MONTHLY_KEEP: document.getElementById('bkMonthlyKeep').value.trim(),
      BACKUP_S3_ENABLED: document.getElementById('bkS3Enabled').checked ? 'true' : 'false',
      BACKUP_S3_ENDPOINT: document.getElementById('bkS3Endpoint').value.trim(),
      BACKUP_S3_REGION: document.getElementById('bkS3Region').value.trim(),
      BACKUP_S3_BUCKET: document.getElementById('bkS3Bucket').value.trim(),
      BACKUP_S3_PREFIX: document.getElementById('bkS3Prefix').value.trim(),
      BACKUP_S3_USE_SSL: document.getElementById('bkS3UseSSL').checked ? 'true' : 'false',
      BACKUP_R2_ENABLED: document.getElementById('bkR2Enabled').checked ? 'true' : 'false',
      BACKUP_R2_BUCKET: document.getElementById('bkR2Bucket').value.trim(),
      BACKUP_R2_PREFIX: document.getElementById('bkR2Prefix').value.trim(),
      BACKUP_TELEGRAM_ENABLED: document.getElementById('bkTgEnabled').checked ? 'true' : 'false',
      BACKUP_TELEGRAM_CHAT_ID: document.getElementById('bkTgChatId').value.trim()
    };
    // فیلدهای محرمانه: فقط اگر کاربر واقعاً چیز تازه‌ای تایپ کرده بفرست، وگرنه
    // مقدار قبلی دست‌نخورده بماند (چون سرور برای هر کلید موجود در body آن را
    // با همان مقدار (حتی خالی) جایگزین می‌کند).
    var passphrase = document.getElementById('bkPassphrase').value;
    if (passphrase !== '') body.BACKUP_PASSPHRASE = passphrase;
    var s3Access = document.getElementById('bkS3AccessKey').value;
    if (s3Access !== '' && s3Access !== '(set)') body.BACKUP_S3_ACCESS_KEY = s3Access;
    var s3Secret = document.getElementById('bkS3SecretKey').value;
    if (s3Secret !== '') body.BACKUP_S3_SECRET_KEY = s3Secret;
    var r2Access = document.getElementById('bkR2AccessKey').value;
    if (r2Access !== '' && r2Access !== '(set)') body.BACKUP_R2_ACCESS_KEY = r2Access;
    var r2Secret = document.getElementById('bkR2SecretKey').value;
    if (r2Secret !== '') body.BACKUP_R2_SECRET_KEY = r2Secret;
    var tgToken = document.getElementById('bkTgBotToken').value;
    if (tgToken !== '') body.BACKUP_TELEGRAM_BOT_TOKEN = tgToken;

    api('/api/backup/settings', body).then(function(data){
      showMessage(data.message || 'Saved', 'success');
      document.getElementById('bkPassphrase').value = '';
      document.getElementById('bkS3SecretKey').value = '';
      document.getElementById('bkR2SecretKey').value = '';
      document.getElementById('bkTgBotToken').value = '';
      loadBackupSettings();
    }).catch(function(err){
      showMessage(err.message, 'danger');
    });
  };

  window.runBackupNow = function(){
    showMessage('Running backup…', 'success');
    api('/api/backup/run', { cadence: 'manual' }).then(function(data){
      showMessage(data.message || 'Backup done', 'success');
      loadBackupSettings();
    }).catch(function(err){
      showMessage(err.message, 'danger');
    });
  };

  window.createR2Bucket = function(){
    var bucket = document.getElementById('bkR2Bucket').value.trim();
    if (!bucket){ showMessage('Enter a bucket name first', 'danger'); return; }
    api('/api/backup/r2/create_bucket', { bucket: bucket }).then(function(data){
      showMessage(data.message || 'Bucket created', 'success');
      loadBackupSettings();
    }).catch(function(err){
      showMessage(err.message, 'danger');
    });
  };

  async function loadDashboardFrame(){
    var frame = document.getElementById('dashboardFrame');
    if (!frame) return;
    var secret = '';
    if (lastTemplate){
      try {
        var clash = lastTemplate.experimental.clash_api;
        if (clash.secret) secret = clash.secret;
      } catch (e){ /* template not loaded yet, use defaults */ }
    }

    var base = await resolvePublicBaseUrl();
    
    // For fallback URL, we need to extract host and port from the base URL
    var p = parseUrlParts(base);
    var host = p.host || window.location.hostname;
    var port = p.port || (p.scheme === 'https' ? '443' : '80');

    var fallbackUrl = 'https://metacubexd.pages.dev/#/setup?hostname=' + encodeURIComponent(host) + '&port=' + encodeURIComponent(port) + '&secret=' + encodeURIComponent(secret);
    var localUrl = base + '/singbox/ui/#/setup?hostname=' + encodeURIComponent(host) + '&port=' + encodeURIComponent(port) + '&secret=' + encodeURIComponent(secret);

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
// DockerService یک وب‌سرویس جدا (کانتینر Docker، مثل zai/grok/deepseek) را نشان
// می‌دهد که همیشه — مستقل از mode تونل Cloudflare — زیر پیشوند مسیر "/" + Name
// روی reverse proxy محلی (پورت proxyPort، پیش‌فرض 80) در دسترس است. آدرس عمومی
// همیشه قطعی است: <دامنه یا هاست‌نیم فعلی>/<Name>/...  — نیازی به override دستی نیست.
type DockerService struct {
	Name string `json:"name"` // پیشوند مسیر عمومی، مثل "zai" → /zai/*
	Port int    `json:"port"` // پورتی که کانتینر روی 127.0.0.1 منتشر کرده، مثل 3000
}

// Subscription یک منبع subscription پراکسی است: یک URL که به‌صورت دوره‌ای/دستی
// fetch می‌شود، یا محتوایی که مستقیم در UI/API پیست شده. خروجی پارس‌شده (لیست
// outbound به شکل sing-box) هرگز اینجا یا در template.json ذخیره نمی‌شود — در
// فایل جدای subscriptionsDir/<Name>.json نگه‌داری می‌شود (رجوع کنید به
// refreshSubscription/renderConfig) تا template.json دستی کوچک و تمیز بماند.
type ServiceDef struct {
	Name       string `json:"name"`
	ListenPort int    `json:"listen_port"`
	ProxyPort  int    `json:"proxy_port"`
}

type Subscription struct {
	Name string `json:"name"` // شناسه‌ی یکتا؛ هم برای نام‌گذاری گروه (Name-auto) و هم پیشوند تگ (Name/tag) استفاده می‌شود
	// SourceType: "remote" (Content نادیده گرفته می‌شود، URL هر بار fetch می‌شود)
	// یا "local" (URL نادیده گرفته می‌شود، محتوای پیست‌شده در Content ذخیره است).
	SourceType string `json:"source_type"`
	URL        string `json:"url,omitempty"`
	Content    string `json:"content,omitempty"`
	Enabled    bool   `json:"enabled"`

	// وضعیت آخرین رفرش — فقط برای نمایش در UI، توسط refreshSubscription پر می‌شود.
	NodeCount   int    `json:"node_count"`
	LastFetched string `json:"last_fetched,omitempty"` // RFC3339
	LastError   string `json:"last_error,omitempty"`
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
	GlobalWarpEndpoint string `json:"global_warp_endpoint,omitempty"`
	BackupWarpEndpoints []string `json:"backup_warp_endpoints,omitempty"`
	// AdminToken در صورت تنظیم از صفحه‌ی Settings، بر متغیر محیطی ADMIN_TOKEN اولویت دارد.
	AdminToken string `json:"admin_token,omitempty"`
	// Cloudflare تنظیمات تونل (هر سه حالت) را نگه می‌دارد.
	Cloudflare CloudflareConfig `json:"cloudflare,omitempty"`
	// DockerServices برای سازگاری با نسخه‌های قبل نگه‌داشته شده است و در
	// readStateOrDefault به Services منتقل می‌شود.
	DockerServices []DockerService `json:"docker_services,omitempty"`
	// Services فهرست وب‌سرویس‌ها و پروکسی‌های اختصاصی (zai/grok/deepseek و غیره)
	Services []ServiceDef `json:"services,omitempty"`
	// Subscriptions فهرست منابع subscription (remote URL یا محتوای local پیست‌شده) است.
	// خروجی پارس‌شده‌ی هر کدام در subscriptionsDir/<name>.json نگه‌داری می‌شود، نه اینجا
	// (اینجا فقط متادیتا/تنظیمات منبع است).
	Subscriptions []Subscription `json:"subscriptions,omitempty"`
	// EnvOverrides مقادیر تنظیمات مبتنی‌بر env که از صفحه‌ی Settings تغییر داده‌شده‌اند
	// (اولویت با این مقادیر است، سپس متغیر محیطی واقعی، سپس مقدار پیش‌فرض کد).
	EnvOverrides map[string]string `json:"env_overrides,omitempty"`
}

func readStateOrDefault() AppState {
	var s AppState
	if err := readJSON(stateFile, &s); err != nil {
		return AppState{}
	}

	// Migrate legacy DockerServices to Services
	if len(s.DockerServices) > 0 {
		for _, ds := range s.DockerServices {
			s.Services = append(s.Services, ServiceDef{
				Name:       ds.Name,
				ListenPort: ds.Port,
				ProxyPort:  0,
			})
		}
		s.DockerServices = nil
		_ = writeState(s) // save immediately after migration
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
            "external_ui": "metacubexd",
            "external_ui_download_url": "https://github.com/MetaCubeX/metacubexd/archive/refs/heads/gh-pages.zip",
            "external_ui_download_detour": "direct",
            "default_mode": "rule",
			"secret": "__CLASH_SECRET__"
        },
        "cache_file": {
            "enabled": true,
            "path": "cache.db",
            "store_fakeip": true,
            "store_rdrc": true
        }
	}
}`

// defaultTemplateRich پایه‌ی کامل و آماده‌ی تولید است — دقیقاً همان ساختاری که
// برای استقرار واقعی استفاده می‌شود (DNS، rule_setهای ir/ads/private، این‌باندهای
// global/auto/direct و ...). فقط در bootstrap نصب تازه به کار می‌رود.
// __CLASH_SECRET__ در زمان اجرا با یک secret تصادفی جایگزین می‌شود.
const defaultTemplateRich = `{
  "log": {
    "disabled": false,
    "level": "fatal",
    "timestamp": true
  },
  "dns": {
    "final": "local-dns",
    "rules": [
      {
        "action": "route",
        "clash_mode": "Global",
        "server": "proxy-dns",
        "source_ip_cidr": ["172.19.0.0/30", "fdfe:dcba:9876::1/126"]
      },
      {
        "action": "route",
        "server": "proxy-dns",
        "source_ip_cidr": ["172.19.0.0/30", "fdfe:dcba:9876::1/126"]
      },
      {
        "action": "route",
        "clash_mode": "Direct",
        "server": "direct-dns"
      },
      {
        "action": "route",
        "rule_set": ["geosite-ir"],
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
      "access_control_allow_origin": ["*"],
      "access_control_allow_private_network": true,
      "external_controller": "127.0.0.1:__CLASH_API_PORT__",
      "external_ui": "metacubexd",
      "external_ui_download_url": "https://github.com/MetaCubeX/metacubexd/archive/refs/heads/gh-pages.zip",
      "external_ui_download_detour": "direct",
      "default_mode": "rule",
	  "secret": "__CLASH_SECRET__"
    },
    "cache_file": {
      "enabled": true,
      "path": "cache.db",
      "store_fakeip": true,
      "store_rdrc": true
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
      "outbounds": ["auto", "direct"],
      "tag": "proxy",
      "type": "selector"
    },
    {
      "interval": "10m",
      "outbounds": ["direct"],
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
        "inbound": ["in-auto"],
        "outbound": "auto"
      },
      {
        "action": "route",
        "inbound": ["in-direct"],
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
        "rule_set": ["geosite-ads"]
      }
    ]
  }
}`

func ensureDefaultFiles() {
	_, tmplErr := os.Stat(templateFile)
	_, nodesErr := os.Stat(nodesFile)

	if os.IsNotExist(tmplErr) && os.IsNotExist(nodesErr) {
		bootstrapFreshInstall()
	} else {
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

	// subscriptionsDir میزبان فایل‌های کش هر subscription است (بخش "منطق کسب‌وکار
	// Subscriptions") — هرگز دستی ویرایش نمی‌شود، فقط توسط refreshSubscription نوشته
	// و توسط renderConfig خوانده می‌شود.
	if err := os.MkdirAll(subscriptionsDir, 0755); err != nil {
		log.Printf("Failed to create subscriptions directory %q: %v", subscriptionsDir, err)
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

	clashSecret := getEnvDefault("CLASH_SECRET", "")
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
		configs, genErr := GenerateWireGuardConfigs("WARP", account, []string{getGlobalWarpEndpoint()})
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

	defaultServices := []ServiceDef{
		{Name: "mimo", ListenPort: 3003, ProxyPort: 2003},
		{Name: "kimi", ListenPort: 3002, ProxyPort: 2002},
		{Name: "deepseek", ListenPort: 3005, ProxyPort: 2005},
		{Name: "zai", ListenPort: 3001, ProxyPort: 2001},
		{Name: "grok2api", ListenPort: 3004, ProxyPort: 2004},
		{Name: "flaresolverr", ListenPort: 8191, ProxyPort: 0},
	}
	// Add user-defined default proxy services (ProxyPort only)
	for _, svc := range parseDefaultServices() {
		// check if it overrides an existing default by name
		found := false
		for i, ds := range defaultServices {
			if ds.Name == svc.Name {
				defaultServices[i].ProxyPort = svc.Port
				found = true
				break
			}
		}
		if !found {
			defaultServices = append(defaultServices, ServiceDef{
				Name:       svc.Name,
				ListenPort: 0, // Proxy-only services don't need a listen port initially, or it can be left 0
				ProxyPort:  svc.Port,
			})
		}
	}
	state.Services = defaultServices

	syncServicesToTemplate(state, tmpl)

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
	if runtime.GOOS == "windows" {
		// پاکسازی پروسه‌های یتیم قبلی sing-box که از اجراهای قبلی/کرش‌ها قفل روی cache.db مانده‌اند
		binName := filepath.Base(path)
		_ = exec.Command("taskkill", "/F", "/IM", binName).Run()
		time.Sleep(100 * time.Millisecond)
	}

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

// stopProcess به‌آرامی پروسه را متوقف می‌کند و در صورت نیاز آن را Kill می‌کند.
// Wait() فقط یک‌بار و فقط توسط گوروتین راه‌اندازی‌شده در startSingBoxLocked فراخوانی می‌شود.
func stopProcess(mp *managedProcess) {
	if mp == nil || mp.cmd == nil || mp.cmd.Process == nil {
		return
	}
	if runtime.GOOS == "windows" {
		// در ویندوز os.Interrupt روی پروسه‌های فرزند exec.Command کار نمی‌کند.
		// با taskkill پروسه را بلافاصله بسته‌شده و قفل فایل کش رها می‌شود.
		_ = exec.Command("taskkill", "/F", "/T", "/PID", fmt.Sprint(mp.cmd.Process.Pid)).Run()
		_ = mp.cmd.Process.Kill()
	} else {
		_ = mp.cmd.Process.Signal(os.Interrupt)
	}
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

// computeTunnelRoutes فهرست ingress را می‌سازد: پنل مدیریت + داشبورد Clash + یک
// route واحد برای کل دامنه به‌سمت reverse proxy محلی (پورت proxyPort). دیگر
// به‌ازای هر DockerService یک ingress/DNS جدا لازم نیست — تفکیک سرویس‌ها
// (zai/grok/deepseek و غیره) حالا با پیشوند مسیر و داخل خودِ reverse proxy
// (بخش "Reverse proxy عمومی") انجام می‌شود، نه در ingress تونل. عمداً
// پروکسی‌های جدول Services (سینگ‌باکس mixed inbound) اینجا نیستند — طبق
// تصمیم صریح شما فقط پروکسی‌ها private می‌مانند.
func computeTunnelRoutes(state AppState) []ingressRoute {
	domain := strings.TrimSuffix(strings.TrimSpace(state.Cloudflare.ZoneName), ".")
	if domain == "" {
		return nil
	}
	routes := []ingressRoute{
		{Key: "panel", Hostname: "panel." + domain, Service: "http://127.0.0.1" + apiPort},
		{Key: "apps", Hostname: domain, Service: "http://127.0.0.1:" + proxyPort},
	}
	if clashAddr, err := getClashAPIAddr(); err == nil {
		routes = append(routes, ingressRoute{Key: "dash", Hostname: "dash." + domain, Service: "http://" + clashAddr})
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
			// "panel" به reverse proxy محلی (نه مستقیم به apiPort) اشاره می‌کند: چون
			// یک Quick Tunnel فقط یک مقصد دارد، این یک هاست‌نیم trycloudflare.com هم
			// پنل مدیریت (روی "/") و هم همه‌ی DockerServiceها (زیر پیشوند مسیرشان،
			// مثل "/zai/") را همزمان سرو می‌کند — به cf_push_ingress نیازی نیست.
			"panel": "http://127.0.0.1:" + proxyPort,
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
		log.Printf("Quick tunnels running: %v (docker services are reachable under the panel URL, e.g. <panel-url>/<service-name>/)", newURLs)

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

// نکته: پیش‌تر اینجا یک syncCloudflareRoutesAsync بود که بعد از هر add/delete
// DockerService، ingress تونل را در پس‌زمینه به‌روزرسانی می‌کرد. چون حالا فقط
// یک route ثابت ("apps" → 127.0.0.1:proxyPort) برای کل دامنه وجود دارد و
// افزودن/حذف یک DockerService دیگر ingress را عوض نمی‌کند (رجوع کنید به
// computeTunnelRoutes و rebuildProxyRoutes)، آن تابع حذف شد.

// ---------------------------------------------------------------------
// تولید تنظیمات WireGuard برای اکانت‌های WARP
// ---------------------------------------------------------------------

func getGlobalWarpEndpoint() string {
	ep := readStateOrDefault().GlobalWarpEndpoint
	if ep == "" {
		return "engage.cloudflareclient.com:2408"
	}
	return ep
}

func groupPrefixForTag(tag string) (prefix string, grouped bool) {
    if strings.HasPrefix(tag, "WARP-") {
        return "WARP", true
    }
	return tag, false
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

	// نودهای subscription (بخش "منطق کسب‌وکار Subscriptions") دقیقاً هم‌شکل خروجی
	// حلقه‌ی بالا (WARP) هستند — به همان چهار متغیر append می‌شوند تا تمام منطق
	// پایین‌دست (globalAutoMembers، selectorOptions، fallbackDefault) بدون هیچ
	// تغییری هم برای WARP و هم برای subscriptionها کار کند.
	subEndpoints, subProxies, subGroupAutoOutbounds, subGroupAutoTags := loadSubscriptionOutbounds(state.Subscriptions, urltestDefaults)
	wireguardNodes = append(wireguardNodes, subEndpoints...)
	otherNodes = append(otherNodes, subProxies...)
	groupAutoOutbounds = append(groupAutoOutbounds, subGroupAutoOutbounds...)
	groupAutoTags = append(groupAutoTags, subGroupAutoTags...)
	cfg["endpoints"] = wireguardNodes

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
	addOpt("direct")
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

// applyCurrentTemplateAndNodes رندر/اعتبارسنجی/اعمال کانفیگ را با محتوای فعلیِ
// روی دیسکِ template.json/nodes.json دوباره اجرا می‌کند، بدون این‌که خودشان را
// تغییر دهد. برای مواردی مثل add/refresh/delete یک subscription لازم است: آن
// عملیات‌ها template.json/nodes.json را دست نمی‌زنند، اما renderConfig هم آن دو
// فایل و هم subscriptionsDir را می‌خواند، پس باید دوباره صدا زده شود تا نودهای
// تازه‌ی subscription واقعاً به config.json در حال اجرا برسند.
func applyCurrentTemplateAndNodes() error {
	tmplRaw, err := os.ReadFile(templateFile)
	if err != nil {
		return fmt.Errorf("failed to read %s: %w", templateFile, err)
	}
	nodesRaw, err := os.ReadFile(nodesFile)
	if err != nil {
		return fmt.Errorf("failed to read %s: %w", nodesFile, err)
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
	state := readStateOrDefault()
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"template": string(tmplData),
		"nodes":    string(nodesData),
		"services": state.Services,
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

// syncServicesToTemplate updates sing-box inbounds, outbounds, and route rules
// based on the current state.Services. It only creates proxies for services
// that have ProxyPort > 0.
func syncServicesToTemplate(state AppState, tmpl map[string]interface{}) {
	defaultTarget := "auto"
	if state.DefaultWarpGroup != "" {
		defaultTarget = state.DefaultWarpGroup + "-auto"
	}

	// 1. Clean existing service inbounds/outbounds/rules
	var newInbounds []interface{}
	for _, in := range asSlice(tmpl["inbounds"]) {
		inMap, ok := in.(map[string]interface{})
		if !ok {
			continue
		}
		tag, _ := inMap["tag"].(string)
		if strings.HasPrefix(tag, "in-") && tag != "in-global" && tag != "in-auto" && tag != "in-direct" {
			continue // remove old service inbounds
		}
		newInbounds = append(newInbounds, in)
	}

	var newOutbounds []interface{}
	for _, out := range asSlice(tmpl["outbounds"]) {
		outMap, ok := out.(map[string]interface{})
		if !ok {
			continue
		}
		tag, _ := outMap["tag"].(string)
		if strings.HasPrefix(tag, "select-") {
			continue // remove old service outbounds
		}
		newOutbounds = append(newOutbounds, out)
	}

	if route, ok := tmpl["route"].(map[string]interface{}); ok {
		var newRules []interface{}
		for _, rule := range asSlice(route["rules"]) {
			ruleMap, _ := rule.(map[string]interface{})
			isServiceRule := false
			for _, tagAny := range asSlice(ruleMap["inbound"]) {
				tag, _ := tagAny.(string)
				if strings.HasPrefix(tag, "in-") && tag != "in-global" && tag != "in-auto" && tag != "in-direct" {
					isServiceRule = true
					break
				}
			}
			if !isServiceRule {
				newRules = append(newRules, rule)
			}
		}
		route["rules"] = newRules
		tmpl["route"] = route
	}

	// 2. Add services with ProxyPort > 0
	for _, svc := range state.Services {
		if svc.ProxyPort <= 0 {
			continue
		}

		inboundTag := "in-" + svc.Name
		selectorTag := "select-" + svc.Name

		newInbounds = append(newInbounds, map[string]interface{}{
			"tag":         inboundTag,
			"listen":      "127.0.0.1",
			"listen_port": svc.ProxyPort,
			"type":        "mixed",
		})

		sel := map[string]interface{}{
			"tag":       selectorTag,
			"type":      "selector",
			"outbounds": []interface{}{"auto"},
		}
		if defaultTarget != "" {
			sel["default"] = defaultTarget
		}
		newOutbounds = append(newOutbounds, sel)

		// Rule to map inbound to outbound
		if route, ok := tmpl["route"].(map[string]interface{}); ok {
			rules := asSlice(route["rules"])
			rules = append(rules, map[string]interface{}{
				"inbound":  []interface{}{inboundTag},
				"outbound": selectorTag,
			})
			route["rules"] = rules
			tmpl["route"] = route
		}
	}

	tmpl["inbounds"] = newInbounds
	tmpl["outbounds"] = newOutbounds
}

func addServiceHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name       string `json:"name"`
		ListenPort int    `json:"listen_port"`
		ProxyPort  int    `json:"proxy_port,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("addServiceHandler: invalid JSON: %v", err)
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Invalid JSON"})
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	log.Printf("addServiceHandler: name=%s, listen_port=%d, proxy_port=%d", req.Name, req.ListenPort, req.ProxyPort)

	if !serviceNameRe.MatchString(req.Name) {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Service name must be 1-32 characters: letters, digits, underscore, hyphen"})
		return
	}
	if req.ListenPort < 1 || req.ListenPort > 65535 {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Invalid listen port (1-65535)"})
		return
	}
	if req.ProxyPort != 0 && (req.ProxyPort < 1 || req.ProxyPort > 65535) {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Invalid proxy port (1-65535)"})
		return
	}
	if reservedServiceNames[req.Name] {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": fmt.Sprintf("%q is a reserved name and cannot be used for a service", req.Name)})
		return
	}

	mu.Lock()
	defer mu.Unlock()

	state := readStateOrDefault()
	for _, s := range state.Services {
		if s.Name == req.Name {
			jsonResponse(w, http.StatusConflict, map[string]interface{}{"error": fmt.Sprintf("Service %q already exists", req.Name)})
			return
		}
		if s.ListenPort == req.ListenPort {
			jsonResponse(w, http.StatusConflict, map[string]interface{}{"error": fmt.Sprintf("Listen port %d is already in use by %q", req.ListenPort, s.Name)})
			return
		}
		if req.ProxyPort != 0 && s.ProxyPort == req.ProxyPort {
			jsonResponse(w, http.StatusConflict, map[string]interface{}{"error": fmt.Sprintf("Proxy port %d is already in use by %q", req.ProxyPort, s.Name)})
			return
		}
	}

	state.Services = append(state.Services, ServiceDef{
		Name:       req.Name,
		ListenPort: req.ListenPort,
		ProxyPort:  req.ProxyPort,
	})
	if err := writeState(state); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"error": "Failed to save state: " + err.Error()})
		return
	}

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

	syncServicesToTemplate(state, tmpl)

	if err := applyChangeFromStruct(tmpl, nodes); err != nil {
		log.Printf("addServiceHandler: applyChange: %v", err)
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": err.Error()})
		return
	}
	rebuildProxyRoutes()
	jsonResponse(w, http.StatusOK, map[string]interface{}{"message": "Service added successfully!"})
}

func deleteServiceHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Invalid JSON"})
		return
	}
	req.Name = strings.TrimSpace(req.Name)

	mu.Lock()
	defer mu.Unlock()

	state := readStateOrDefault()
	found := false
	var kept []ServiceDef
	for _, s := range state.Services {
		if s.Name == req.Name {
			found = true
			continue
		}
		kept = append(kept, s)
	}

	if !found {
		jsonResponse(w, http.StatusNotFound, map[string]interface{}{"error": fmt.Sprintf("Service %q not found", req.Name)})
		return
	}

	state.Services = kept
	if err := writeState(state); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"error": "Failed to save state: " + err.Error()})
		return
	}

	var tmpl map[string]interface{}
	if err := readJSON(templateFile, &tmpl); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"error": "Failed to read template.json"})
		return
	}
	var nodes []interface{}
	if err := readJSON(nodesFile, &nodes); err != nil && !os.IsNotExist(err) {
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"error": "Failed to read nodes.json"})
		return
	}

	syncServicesToTemplate(state, tmpl)

	if err := applyChangeFromStruct(tmpl, nodes); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": err.Error()})
		return
	}
	rebuildProxyRoutes()
	jsonResponse(w, http.StatusOK, map[string]interface{}{"message": fmt.Sprintf("Service '%s' deleted successfully!", req.Name)})
}

// editServiceHandler نام و/یا پورت یک سرویس موجود را تغییر می‌دهد: تگ inbound،
// تگ selector مرتبط، و ارجاع‌های route rule را هماهنگ به‌روزرسانی می‌کند.
func editServiceHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OldName       string `json:"old_name"`
		NewName       string `json:"new_name"`
		NewListenPort int    `json:"new_listen_port"`
		NewProxyPort  int    `json:"new_proxy_port,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Invalid JSON"})
		return
	}
	req.OldName = strings.TrimSpace(req.OldName)
	req.NewName = strings.TrimSpace(req.NewName)

	if req.OldName == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "old_name is required"})
		return
	}
	if !serviceNameRe.MatchString(req.NewName) {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Service name must be 1-32 characters: letters, digits, underscore, hyphen"})
		return
	}
	if req.NewListenPort < 1 || req.NewListenPort > 65535 {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Invalid listen port (1-65535)"})
		return
	}
	if req.NewProxyPort != 0 && (req.NewProxyPort < 1 || req.NewProxyPort > 65535) {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Invalid proxy port (1-65535)"})
		return
	}
	if req.NewName != req.OldName && reservedServiceNames[req.NewName] {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": fmt.Sprintf("%q is a reserved name", req.NewName)})
		return
	}

	mu.Lock()
	defer mu.Unlock()

	state := readStateOrDefault()

	// Validation pass
	targetIdx := -1
	for i, s := range state.Services {
		if s.Name == req.OldName {
			targetIdx = i
		} else {
			if s.Name == req.NewName {
				jsonResponse(w, http.StatusConflict, map[string]interface{}{"error": fmt.Sprintf("Service %q already exists", req.NewName)})
				return
			}
			if s.ListenPort == req.NewListenPort {
				jsonResponse(w, http.StatusConflict, map[string]interface{}{"error": fmt.Sprintf("Listen port %d is already in use by %q", req.NewListenPort, s.Name)})
				return
			}
			if req.NewProxyPort != 0 && s.ProxyPort == req.NewProxyPort {
				jsonResponse(w, http.StatusConflict, map[string]interface{}{"error": fmt.Sprintf("Proxy port %d is already in use by %q", req.NewProxyPort, s.Name)})
				return
			}
		}
	}

	if targetIdx == -1 {
		jsonResponse(w, http.StatusNotFound, map[string]interface{}{"error": fmt.Sprintf("Service %q not found", req.OldName)})
		return
	}

	state.Services[targetIdx] = ServiceDef{
		Name:       req.NewName,
		ListenPort: req.NewListenPort,
		ProxyPort:  req.NewProxyPort,
	}

	if err := writeState(state); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"error": "Failed to save state: " + err.Error()})
		return
	}

	var tmpl map[string]interface{}
	if err := readJSON(templateFile, &tmpl); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"error": "Failed to read template.json"})
		return
	}
	var nodes []interface{}
	if err := readJSON(nodesFile, &nodes); err != nil && !os.IsNotExist(err) {
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"error": "Failed to read nodes.json"})
		return
	}

	syncServicesToTemplate(state, tmpl)

	if err := applyChangeFromStruct(tmpl, nodes); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": err.Error()})
		return
	}
	rebuildProxyRoutes()
	jsonResponse(w, http.StatusOK, map[string]interface{}{"message": fmt.Sprintf("Service %q updated", req.NewName)})
}

// ---------------------------------------------------------------------
// پارسر subscription: تبدیل محتوای remote/local (هر کدام از فرمت‌های sing-box
// JSON، Clash JSON/YAML، خطوط URI، خطوط ساده‌ی IP:PORT، یا base64 روی هرکدام از
// این‌ها) به لیستی از outbound به شکل sing-box. الگوی طراحی از GeneralSubscriptionParser
// کتابخانه‌ی Resin (github.com/Resinat/Resin) گرفته شده؛ خودِ کد اینجا از صفر
// نوشته شده چون Resin یک گیت‌وی کامل و مستقل است (sing-box را به‌عنوان کتابخانه
// import می‌کند و SQLite/health-check/sticky-routing خودش را دارد)، نه یک
// کتابخانه‌ی سبک قابل import — معماری این پروژه (manager دور یک باینری خارجی
// sing-box) با آن سازگار نیست.
// ---------------------------------------------------------------------

// subSupportedOutboundTypes: نوع outboundهایی که مستقیم (بدون تبدیل) از یک
// subscription به شکل sing-box JSON پذیرفته می‌شوند. هر چیز دیگری (selector،
// urltest، direct، block، dns، یا هر type ناشناخته) نادیده گرفته می‌شود — این‌ها
// outbound "نود" واقعی نیستند.
var subSupportedOutboundTypes = map[string]bool{
	"socks": true, "http": true, "shadowsocks": true, "vmess": true, "trojan": true,
	"wireguard": true, "hysteria": true, "vless": true, "shadowtls": true, "tuic": true,
	"hysteria2": true, "anytls": true, "ssh": true,
}

// subscriptionNameRe نام subscription را محدود می‌کند — چون هم برای نام فایل
// کش (subscriptionsDir/<name>.json) و هم پیشوند گروه/تگ ("<name>-auto"،
// "<name>/<tag>") استفاده می‌شود.
var subscriptionNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)

// parseGeneralSubscription نقطه‌ی ورود پارسر است: فرمت محتوا را تشخیص می‌دهد و
// لیست outbound (هرکدام حداقل "type" و "tag" دارد) برمی‌گرداند. نودهایی که تک‌تک
// پارس‌شان شکست بخورد (نه کل subscription) با یک warning رد می‌شوند — یک خط
// خراب در subscriptionی با ۵۰۰ نود نباید ۴۹۹ تای دیگر را هم از بین ببرد.
func parseGeneralSubscription(raw []byte) (outbounds []map[string]interface{}, warnings []string, err error) {
	content := unwrapBase64IfWrapped(raw)
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) == 0 {
		return nil, nil, fmt.Errorf("subscription content is empty")
	}

	if trimmed[0] == '{' {
		var obj map[string]interface{}
		if jsonErr := json.Unmarshal(trimmed, &obj); jsonErr == nil {
			if rawList, ok := obj["outbounds"].([]interface{}); ok {
				ob, warn := filterSupportedOutbounds(rawList)
				return ob, warn, nil
			}
			if proxies, ok := obj["proxies"].([]interface{}); ok {
				ob, warn := convertClashProxies(proxies)
				return ob, warn, nil
			}
			return nil, nil, fmt.Errorf("JSON object has neither \"outbounds\" nor \"proxies\"")
		}
	}
	if trimmed[0] == '[' {
		var arr []interface{}
		if jsonErr := json.Unmarshal(trimmed, &arr); jsonErr == nil {
			ob, warn := filterSupportedOutbounds(arr)
			return ob, warn, nil
		}
	}

	if looksLikeYAML(trimmed) {
		var doc map[string]interface{}
		if yamlErr := yaml.Unmarshal(trimmed, &doc); yamlErr == nil {
			if proxies, ok := doc["proxies"].([]interface{}); ok {
				ob, warn := convertClashProxies(proxies)
				return ob, warn, nil
			}
		}
	}

	return parseLineBasedSubscription(string(trimmed))
}

func looksLikeYAML(b []byte) bool {
	s := string(b)
	return strings.Contains(s, "\nproxies:") || strings.HasPrefix(s, "proxies:")
}

var subURISchemes = []string{"vmess", "vless", "trojan", "ss", "hysteria2", "hy2", "http", "https", "socks5", "socks5h"}
var reIPv4 = regexp.MustCompile(`^\d{1,3}(\.\d{1,3}){3}$`)

// unwrapBase64IfWrapped: اگر کل محتوا از قبل شبیه JSON/YAML/خط URI نباشد، چند
// نوع رمزگشایی base64 را امتحان می‌کند — بسیاری از ارائه‌دهنده‌های subscription
// کل لیست نود را این‌طور می‌پیچند.
func unwrapBase64IfWrapped(raw []byte) []byte {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return raw
	}
	if trimmed[0] == '{' || trimmed[0] == '[' || looksLikeYAML(trimmed) || looksLikeURILines(trimmed) {
		return raw
	}
	compact := string(bytes.Join(bytes.Fields(trimmed), []byte("")))
	decoders := []*base64.Encoding{base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.RawURLEncoding}
	for _, dec := range decoders {
		if out, decErr := dec.DecodeString(compact); decErr == nil && looksLikeDecodedSubscription(out) {
			return out
		}
	}
	return raw
}

func looksLikeURILines(b []byte) bool {
	for _, scheme := range subURISchemes {
		if bytes.Contains(b, []byte(scheme+"://")) {
			return true
		}
	}
	return false
}

func looksLikeDecodedSubscription(b []byte) bool {
	t := bytes.TrimSpace(b)
	if len(t) == 0 {
		return false
	}
	if t[0] == '{' || t[0] == '[' || looksLikeYAML(t) || looksLikeURILines(t) {
		return true
	}
	for _, line := range strings.Split(string(t), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if _, _, _, _, ok := parsePlainHostPortLine(line); ok {
			return true
		}
	}
	return false
}

// filterSupportedOutbounds فقط entryهایی را نگه می‌دارد که "type"شان یک نود
// پراکسی واقعی است (نه "direct"/"block"/"selector"/"urltest"/"dns"/ناشناخته).
func filterSupportedOutbounds(raw []interface{}) (out []map[string]interface{}, warnings []string) {
	for i, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			warnings = append(warnings, fmt.Sprintf("outbound #%d: not a JSON object, skipped", i))
			continue
		}
		t, _ := m["type"].(string)
		if !subSupportedOutboundTypes[t] {
			warnings = append(warnings, fmt.Sprintf("outbound #%d: unsupported type %q, skipped", i, t))
			continue
		}
		out = append(out, m)
	}
	return
}

// ---------------------------------------------------------------------
// تبدیل Clash proxy -> outbound به شکل sing-box
// ---------------------------------------------------------------------

func convertClashProxies(raw []interface{}) (out []map[string]interface{}, warnings []string) {
	for i, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			warnings = append(warnings, fmt.Sprintf("proxy #%d: not an object, skipped", i))
			continue
		}
		ob, convErr := convertClashProxy(m)
		if convErr != nil {
			name, _ := m["name"].(string)
			warnings = append(warnings, fmt.Sprintf("proxy #%d (%s): %v, skipped", i, name, convErr))
			continue
		}
		out = append(out, ob)
	}
	return
}

func csStr(m map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			return fmt.Sprintf("%v", v)
		}
	}
	return ""
}

func csBool(m map[string]interface{}, keys ...string) bool {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch t := v.(type) {
			case bool:
				return t
			case string:
				return t == "true" || t == "1"
			}
		}
	}
	return false
}

func csInt(m map[string]interface{}, def int, keys ...string) int {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch t := v.(type) {
			case int:
				return t
			case float64:
				return int(t)
			case string:
				if n, convErr := strconv.Atoi(t); convErr == nil {
					return n
				}
			}
		}
	}
	return def
}

func csMap(m map[string]interface{}, keys ...string) map[string]interface{} {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if sub, ok2 := v.(map[string]interface{}); ok2 {
				return sub
			}
		}
	}
	return nil
}

func csStrList(m map[string]interface{}, keys ...string) []string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if list, ok2 := v.([]interface{}); ok2 {
				out := make([]string, 0, len(list))
				for _, it := range list {
					out = append(out, fmt.Sprintf("%v", it))
				}
				return out
			}
			if s, ok2 := v.(string); ok2 && s != "" {
				return []string{s}
			}
		}
	}
	return nil
}

// clashTLSBlock یک شیء "tls" به سبک sing-box از فیلدهای رایج TLS در Clash می‌سازد.
func clashTLSBlock(m map[string]interface{}, enabled bool, defaultSNI string) map[string]interface{} {
	sni := csStr(m, "sni", "servername")
	if sni == "" {
		sni = defaultSNI
	}
	tls := map[string]interface{}{
		"enabled":     enabled,
		"server_name": sni,
		"insecure":    csBool(m, "skip-cert-verify", "allow_insecure", "allowInsecure", "insecure"),
	}
	if alpn := csStrList(m, "alpn"); len(alpn) > 0 {
		tls["alpn"] = alpn
	}
	if fp := csStr(m, "client-fingerprint", "fingerprint"); fp != "" {
		tls["utls"] = map[string]interface{}{"enabled": true, "fingerprint": fp}
	}
	if ro := csMap(m, "reality-opts"); ro != nil {
		tls["reality"] = map[string]interface{}{
			"enabled":    true,
			"public_key": csStr(ro, "public-key"),
			"short_id":   csStr(ro, "short-id"),
		}
	}
	return tls
}

// clashTransportBlock یک شیء "transport" به سبک sing-box از فیلدهای
// network/ws-opts/grpc-opts/h2-opts/httpupgrade-opts در Clash می‌سازد. برای tcp
// ساده nil برمی‌گرداند.
func clashTransportBlock(m map[string]interface{}) map[string]interface{} {
	network := csStr(m, "network")
	switch network {
	case "ws":
		opts := csMap(m, "ws-opts")
		t := map[string]interface{}{"type": "ws"}
		if opts != nil {
			t["path"] = csStr(opts, "path")
			if headers := csMap(opts, "headers"); headers != nil {
				t["headers"] = headers
			}
		}
		return t
	case "grpc":
		opts := csMap(m, "grpc-opts")
		t := map[string]interface{}{"type": "grpc"}
		if opts != nil {
			t["service_name"] = csStr(opts, "grpc-service-name")
		}
		return t
	case "h2", "http":
		opts := csMap(m, "h2-opts")
		t := map[string]interface{}{"type": "http"}
		if opts != nil {
			if hosts := csStrList(opts, "host"); len(hosts) > 0 {
				t["host"] = hosts
			}
			t["path"] = csStr(opts, "path")
		}
		return t
	case "httpupgrade":
		opts := csMap(m, "httpupgrade-opts")
		t := map[string]interface{}{"type": "httpupgrade"}
		if opts != nil {
			t["host"] = csStr(opts, "host")
			t["path"] = csStr(opts, "path")
		}
		return t
	}
	return nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func convertClashProxy(m map[string]interface{}) (map[string]interface{}, error) {
	typ := strings.ToLower(csStr(m, "type"))
	tag := csStr(m, "name")
	server := csStr(m, "server")
	port := csInt(m, 0, "port")
	if tag == "" {
		tag = fmt.Sprintf("%s:%d", server, port)
	}
	if server == "" || port == 0 {
		return nil, fmt.Errorf("missing server/port")
	}

	switch typ {
	case "ss", "shadowsocks":
		out := map[string]interface{}{
			"type": "shadowsocks", "tag": tag, "server": server, "server_port": port,
			"method": csStr(m, "cipher", "method"), "password": csStr(m, "password"),
		}
		if plugin := csStr(m, "plugin"); plugin != "" {
			out["plugin"] = plugin
			if po := csMap(m, "plugin-opts"); po != nil {
				var parts []string
				for k, v := range po {
					parts = append(parts, fmt.Sprintf("%s=%v", k, v))
				}
				out["plugin_opts"] = strings.Join(parts, ";")
			}
		}
		return out, nil

	case "socks", "socks4", "socks4a", "socks5":
		return map[string]interface{}{
			"type": "socks", "tag": tag, "server": server, "server_port": port, "version": "5",
			"username": csStr(m, "username"), "password": csStr(m, "password"),
		}, nil

	case "http":
		out := map[string]interface{}{
			"type": "http", "tag": tag, "server": server, "server_port": port,
			"username": csStr(m, "username"), "password": csStr(m, "password"),
		}
		if csBool(m, "tls") {
			out["tls"] = clashTLSBlock(m, true, server)
		}
		return out, nil

	case "vmess":
		out := map[string]interface{}{
			"type": "vmess", "tag": tag, "server": server, "server_port": port,
			"uuid": csStr(m, "uuid"), "security": orDefault(csStr(m, "cipher"), "auto"),
			"alter_id": csInt(m, 0, "alterId", "alter_id"),
		}
		if csBool(m, "tls") {
			out["tls"] = clashTLSBlock(m, true, server)
		}
		if tr := clashTransportBlock(m); tr != nil {
			out["transport"] = tr
		}
		return out, nil

	case "vless":
		out := map[string]interface{}{
			"type": "vless", "tag": tag, "server": server, "server_port": port,
			"uuid": csStr(m, "uuid"),
		}
		if flow := csStr(m, "flow"); flow != "" {
			out["flow"] = flow
		}
		if csBool(m, "tls") {
			out["tls"] = clashTLSBlock(m, true, server)
		}
		if tr := clashTransportBlock(m); tr != nil {
			out["transport"] = tr
		}
		return out, nil

	case "trojan":
		out := map[string]interface{}{
			"type": "trojan", "tag": tag, "server": server, "server_port": port,
			"password": csStr(m, "password"),
			"tls":      clashTLSBlock(m, true, server),
		}
		if tr := clashTransportBlock(m); tr != nil {
			out["transport"] = tr
		}
		return out, nil

	case "hysteria":
		out := map[string]interface{}{
			"type": "hysteria", "tag": tag, "server": server, "server_port": port,
			"up_mbps": csInt(m, 0, "up", "up_mbps"), "down_mbps": csInt(m, 0, "down", "down_mbps"),
			"auth_str": csStr(m, "auth_str", "auth-str"),
			"tls":      clashTLSBlock(m, true, server),
		}
		if obfs := csStr(m, "obfs"); obfs != "" {
			out["obfs"] = obfs
		}
		return out, nil

	case "hysteria2", "hy2":
		out := map[string]interface{}{
			"type": "hysteria2", "tag": tag, "server": server, "server_port": port,
			"password": csStr(m, "password", "auth"),
			"tls":      clashTLSBlock(m, true, server),
		}
		if obfs := csStr(m, "obfs"); obfs != "" {
			out["obfs"] = map[string]interface{}{"type": obfs, "password": csStr(m, "obfs-password")}
		}
		return out, nil

	case "tuic":
		return map[string]interface{}{
			"type": "tuic", "tag": tag, "server": server, "server_port": port,
			"uuid": csStr(m, "uuid"), "password": csStr(m, "password"),
			"congestion_control": orDefault(csStr(m, "congestion-controller", "congestion_control"), "bbr"),
			"tls":                clashTLSBlock(m, true, server),
		}, nil

	case "anytls":
		return map[string]interface{}{
			"type": "anytls", "tag": tag, "server": server, "server_port": port,
			"password": csStr(m, "password"),
			"tls":      clashTLSBlock(m, true, server),
		}, nil

	case "ssh":
		out := map[string]interface{}{
			"type": "ssh", "tag": tag, "server": server, "server_port": port,
			"user": csStr(m, "username", "user"),
		}
		if pw := csStr(m, "password"); pw != "" {
			out["password"] = pw
		}
		if pk := csStr(m, "private-key", "private_key"); pk != "" {
			out["private_key"] = pk
		}
		return out, nil

	case "wireguard", "wg":
		out := map[string]interface{}{
			"type": "wireguard", "tag": tag, "server": server, "server_port": port,
			"private_key":     csStr(m, "private-key", "private_key"),
			"peer_public_key": csStr(m, "public-key", "peer_public_key"),
		}
		if addrs := csStrList(m, "ip", "ipv6", "local-address"); len(addrs) > 0 {
			out["local_address"] = addrs
		}
		return out, nil

	default:
		return nil, fmt.Errorf("unsupported clash proxy type %q", typ)
	}
}

// ---------------------------------------------------------------------
// پارس خط‌محور: خطوط URI + خطوط ساده‌ی IP:PORT
// ---------------------------------------------------------------------

func parseLineBasedSubscription(content string) (out []map[string]interface{}, warnings []string, err error) {
	lines := strings.Split(content, "\n")
	found := 0
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		ob, perr := parseSubscriptionLine(line)
		if perr != nil {
			warnings = append(warnings, fmt.Sprintf("line %d: %v, skipped", i+1, perr))
			continue
		}
		found++
		out = append(out, ob)
	}
	if found == 0 {
		return nil, warnings, fmt.Errorf("no recognizable node lines found")
	}
	return out, warnings, nil
}

func parseSubscriptionLine(line string) (map[string]interface{}, error) {
	switch {
	case strings.HasPrefix(line, "vmess://"):
		return parseVmessURI(line)
	case strings.HasPrefix(line, "vless://"):
		return parseVlessURI(line)
	case strings.HasPrefix(line, "trojan://"):
		return parseTrojanURI(line)
	case strings.HasPrefix(line, "ss://"):
		return parseSSURI(line)
	case strings.HasPrefix(line, "hysteria2://"), strings.HasPrefix(line, "hy2://"):
		return parseHysteria2URI(line)
	case strings.HasPrefix(line, "http://"), strings.HasPrefix(line, "https://"),
		strings.HasPrefix(line, "socks5://"), strings.HasPrefix(line, "socks5h://"):
		return parseSimpleProxyURI(line)
	default:
		if host, port, user, pass, ok := parsePlainHostPortLine(line); ok {
			return plainLineToOutbound(host, port, user, pass), nil
		}
		return nil, fmt.Errorf("unrecognized line format")
	}
}

func tagFromFragment(u *url.URL, fallback string) string {
	if u.Fragment != "" {
		if t, uerr := url.QueryUnescape(u.Fragment); uerr == nil && t != "" {
			return t
		}
		return u.Fragment
	}
	return fallback
}

func b64DecodeLoose(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if m := len(s) % 4; m != 0 {
		s += strings.Repeat("=", 4-m)
	}
	if b, decErr := base64.StdEncoding.DecodeString(s); decErr == nil {
		return b, nil
	}
	return base64.URLEncoding.DecodeString(s)
}

// --- vmess:// (سبک v2rayN: base64 روی یک JSON) ---
func parseVmessURI(line string) (map[string]interface{}, error) {
	payload := strings.TrimPrefix(line, "vmess://")
	raw, derr := b64DecodeLoose(payload)
	if derr != nil {
		return nil, fmt.Errorf("vmess: base64 decode failed: %w", derr)
	}
	var v map[string]interface{}
	if jerr := json.Unmarshal(raw, &v); jerr != nil {
		return nil, fmt.Errorf("vmess: JSON decode failed: %w", jerr)
	}
	server := csStr(v, "add")
	port := csInt(v, 0, "port")
	if server == "" || port == 0 {
		return nil, fmt.Errorf("vmess: missing add/port")
	}
	tag := csStr(v, "ps")
	if tag == "" {
		tag = fmt.Sprintf("%s:%d", server, port)
	}
	out := map[string]interface{}{
		"type": "vmess", "tag": tag, "server": server, "server_port": port,
		"uuid": csStr(v, "id"), "security": orDefault(csStr(v, "scy"), "auto"),
		"alter_id": csInt(v, 0, "aid"),
	}
	if tlsMode := csStr(v, "tls"); tlsMode != "" {
		sni := orDefault(csStr(v, "sni"), csStr(v, "host"))
		tls := map[string]interface{}{"enabled": true, "server_name": orDefault(sni, server), "insecure": false}
		if alpn := csStr(v, "alpn"); alpn != "" {
			tls["alpn"] = strings.Split(alpn, ",")
		}
		if fp := csStr(v, "fp"); fp != "" {
			tls["utls"] = map[string]interface{}{"enabled": true, "fingerprint": fp}
		}
		out["tls"] = tls
	}
	switch csStr(v, "net") {
	case "ws":
		out["transport"] = map[string]interface{}{
			"type": "ws", "path": csStr(v, "path"),
			"headers": map[string]interface{}{"Host": csStr(v, "host")},
		}
	case "grpc":
		out["transport"] = map[string]interface{}{"type": "grpc", "service_name": csStr(v, "path")}
	case "h2":
		var hosts []string
		if host := csStr(v, "host"); host != "" {
			hosts = strings.Split(host, ",")
		}
		out["transport"] = map[string]interface{}{"type": "http", "host": hosts, "path": csStr(v, "path")}
	case "httpupgrade":
		out["transport"] = map[string]interface{}{"type": "httpupgrade", "host": csStr(v, "host"), "path": csStr(v, "path")}
	}
	return out, nil
}

// --- vless://uuid@host:port?params#tag ---
func parseVlessURI(line string) (map[string]interface{}, error) {
	u, uerr := url.Parse(line)
	if uerr != nil {
		return nil, fmt.Errorf("vless: %w", uerr)
	}
	uuid := u.User.Username()
	host, portStr, herr := net.SplitHostPort(u.Host)
	if herr != nil {
		return nil, fmt.Errorf("vless: invalid host:port: %w", herr)
	}
	port, _ := strconv.Atoi(portStr)
	if uuid == "" || host == "" || port == 0 {
		return nil, fmt.Errorf("vless: missing uuid/host/port")
	}
	q := u.Query()
	tag := tagFromFragment(u, fmt.Sprintf("%s:%d", host, port))
	out := map[string]interface{}{
		"type": "vless", "tag": tag, "server": host, "server_port": port, "uuid": uuid,
	}
	if flow := q.Get("flow"); flow != "" {
		out["flow"] = flow
	}
	security := q.Get("security")
	if security == "tls" || security == "reality" {
		tls := map[string]interface{}{
			"enabled": true, "server_name": orDefault(q.Get("sni"), host),
			"insecure": q.Get("allowInsecure") == "1" || q.Get("insecure") == "1",
		}
		if alpn := q.Get("alpn"); alpn != "" {
			tls["alpn"] = strings.Split(alpn, ",")
		}
		if fp := q.Get("fp"); fp != "" {
			tls["utls"] = map[string]interface{}{"enabled": true, "fingerprint": fp}
		}
		if security == "reality" {
			tls["reality"] = map[string]interface{}{
				"enabled": true, "public_key": q.Get("pbk"), "short_id": q.Get("sid"),
			}
		}
		out["tls"] = tls
	}
	if tr := uriTransportBlock(q); tr != nil {
		out["transport"] = tr
	}
	return out, nil
}

// --- trojan://password@host:port?params#tag ---
func parseTrojanURI(line string) (map[string]interface{}, error) {
	u, uerr := url.Parse(line)
	if uerr != nil {
		return nil, fmt.Errorf("trojan: %w", uerr)
	}
	password := u.User.Username()
	host, portStr, herr := net.SplitHostPort(u.Host)
	if herr != nil {
		return nil, fmt.Errorf("trojan: invalid host:port: %w", herr)
	}
	port, _ := strconv.Atoi(portStr)
	if password == "" || host == "" || port == 0 {
		return nil, fmt.Errorf("trojan: missing password/host/port")
	}
	q := u.Query()
	tag := tagFromFragment(u, fmt.Sprintf("%s:%d", host, port))
	tls := map[string]interface{}{
		"enabled": q.Get("security") != "none", "server_name": orDefault(q.Get("sni"), host),
		"insecure": q.Get("allowInsecure") == "1" || q.Get("insecure") == "1",
	}
	out := map[string]interface{}{
		"type": "trojan", "tag": tag, "server": host, "server_port": port,
		"password": password, "tls": tls,
	}
	if tr := uriTransportBlock(q); tr != nil {
		out["transport"] = tr
	}
	return out, nil
}

func uriTransportBlock(q url.Values) map[string]interface{} {
	switch q.Get("type") {
	case "ws":
		t := map[string]interface{}{"type": "ws", "path": q.Get("path")}
		if host := q.Get("host"); host != "" {
			t["headers"] = map[string]interface{}{"Host": host}
		}
		return t
	case "grpc":
		return map[string]interface{}{"type": "grpc", "service_name": q.Get("serviceName")}
	case "http", "h2":
		var hosts []string
		if h := q.Get("host"); h != "" {
			hosts = strings.Split(h, ",")
		}
		return map[string]interface{}{"type": "http", "host": hosts, "path": q.Get("path")}
	case "httpupgrade":
		return map[string]interface{}{"type": "httpupgrade", "host": q.Get("host"), "path": q.Get("path")}
	}
	return nil
}

// --- ss://... (فرم SIP002: userinfo پایه۶۴شده، یا فرم قدیمی: کل رشته پایه۶۴شده) ---
func parseSSURI(line string) (map[string]interface{}, error) {
	body := strings.TrimPrefix(line, "ss://")
	tagPart := ""
	if idx := strings.Index(body, "#"); idx >= 0 {
		if t, uerr := url.QueryUnescape(body[idx+1:]); uerr == nil {
			tagPart = t
		} else {
			tagPart = body[idx+1:]
		}
		body = body[:idx]
	}
	query := ""
	if idx := strings.Index(body, "?"); idx >= 0 {
		query = body[idx+1:]
		body = body[:idx]
	}

	var methodPass, hostPort string
	if idx := strings.LastIndex(body, "@"); idx >= 0 {
		userinfo, hp := body[:idx], body[idx+1:]
		hostPort = hp
		if dec, derr := b64DecodeLoose(userinfo); derr == nil && strings.Contains(string(dec), ":") {
			methodPass = string(dec)
		} else {
			methodPass = userinfo
		}
	} else {
		dec, derr := b64DecodeLoose(body)
		if derr != nil {
			return nil, fmt.Errorf("ss: base64 decode failed: %w", derr)
		}
		at := strings.LastIndex(string(dec), "@")
		if at < 0 {
			return nil, fmt.Errorf("ss: decoded form missing '@'")
		}
		methodPass, hostPort = string(dec[:at]), string(dec[at+1:])
	}
	mp := strings.SplitN(methodPass, ":", 2)
	if len(mp) != 2 {
		return nil, fmt.Errorf("ss: malformed method:password")
	}
	host, portStr, herr := net.SplitHostPort(hostPort)
	if herr != nil {
		return nil, fmt.Errorf("ss: invalid host:port: %w", herr)
	}
	port, _ := strconv.Atoi(portStr)
	tag := tagPart
	if tag == "" {
		tag = fmt.Sprintf("%s:%d", host, port)
	}
	out := map[string]interface{}{
		"type": "shadowsocks", "tag": tag, "server": host, "server_port": port,
		"method": mp[0], "password": mp[1],
	}
	if query != "" {
		q, _ := url.ParseQuery(query)
		if plugin := q.Get("plugin"); plugin != "" {
			parts := strings.SplitN(plugin, ";", 2)
			out["plugin"] = parts[0]
			if len(parts) > 1 {
				out["plugin_opts"] = parts[1]
			}
		}
	}
	return out, nil
}

// --- hysteria2://password@host:port?params#tag (hy2:// هم به‌عنوان alias پذیرفته می‌شود) ---
func parseHysteria2URI(line string) (map[string]interface{}, error) {
	line = strings.Replace(line, "hy2://", "hysteria2://", 1)
	u, uerr := url.Parse(line)
	if uerr != nil {
		return nil, fmt.Errorf("hysteria2: %w", uerr)
	}
	password := u.User.Username()
	host, portStr, herr := net.SplitHostPort(u.Host)
	if herr != nil {
		return nil, fmt.Errorf("hysteria2: invalid host:port: %w", herr)
	}
	port, _ := strconv.Atoi(portStr)
	if host == "" || port == 0 {
		return nil, fmt.Errorf("hysteria2: missing host/port")
	}
	q := u.Query()
	tag := tagFromFragment(u, fmt.Sprintf("%s:%d", host, port))
	out := map[string]interface{}{
		"type": "hysteria2", "tag": tag, "server": host, "server_port": port, "password": password,
		"tls": map[string]interface{}{
			"enabled": true, "server_name": orDefault(q.Get("sni"), host),
			"insecure": q.Get("insecure") == "1",
		},
	}
	if obfs := q.Get("obfs"); obfs != "" {
		out["obfs"] = map[string]interface{}{"type": obfs, "password": q.Get("obfs-password")}
	}
	return out, nil
}

// --- http:// https:// socks5:// socks5h://   scheme://[user:pass@]host:port[#tag] ---
func parseSimpleProxyURI(line string) (map[string]interface{}, error) {
	u, uerr := url.Parse(line)
	if uerr != nil {
		return nil, fmt.Errorf("proxy uri: %w", uerr)
	}
	host, portStr, herr := net.SplitHostPort(u.Host)
	if herr != nil {
		return nil, fmt.Errorf("proxy uri: invalid host:port: %w", herr)
	}
	port, _ := strconv.Atoi(portStr)
	if host == "" || port == 0 {
		return nil, fmt.Errorf("proxy uri: missing host/port")
	}
	tag := tagFromFragment(u, fmt.Sprintf("%s:%d", host, port))
	username := u.User.Username()
	password, _ := u.User.Password()
	q := u.Query()

	switch u.Scheme {
	case "http", "https":
		out := map[string]interface{}{
			"type": "http", "tag": tag, "server": host, "server_port": port,
			"username": username, "password": password,
		}
		if u.Scheme == "https" {
			sni := q.Get("sni")
			if sni == "" {
				sni = q.Get("servername")
			}
			if sni == "" {
				sni = q.Get("peer")
			}
			out["tls"] = map[string]interface{}{
				"enabled": true, "server_name": orDefault(sni, host),
				"insecure": q.Get("allowInsecure") == "1" || q.Get("insecure") == "1",
			}
		}
		return out, nil
	case "socks5", "socks5h":
		return map[string]interface{}{
			"type": "socks", "tag": tag, "server": host, "server_port": port, "version": "5",
			"username": username, "password": password,
		}, nil
	}
	return nil, fmt.Errorf("proxy uri: unsupported scheme %q", u.Scheme)
}

// --- خطوط ساده: IP:PORT یا IP:PORT:USER:PASS (IPv4 و IPv6) ---

// parsePlainHostPortLine هم IPv4 و هم IPv6 (با یا بدون براکت) را تشخیص می‌دهد.
// برای IPv6 بدون براکت، چون خودِ آدرس هم ":" دارد، از سمت راست کار می‌کند: آخرین
// ۱ یا ۳ فیلد جداشده‌با‌":" را PORT یا PORT:USER:PASS در نظر می‌گیرد و باقی‌مانده
// را به‌عنوان IP اعتبارسنجی می‌کند.
func parsePlainHostPortLine(line string) (host string, port int, user, pass string, ok bool) {
	if strings.HasPrefix(line, "[") {
		closeIdx := strings.Index(line, "]")
		if closeIdx < 0 {
			return "", 0, "", "", false
		}
		ipPart := line[1:closeIdx]
		if net.ParseIP(ipPart) == nil {
			return "", 0, "", "", false
		}
		rest := strings.TrimPrefix(line[closeIdx+1:], ":")
		return finishPlainLine(ipPart, rest)
	}

	parts := strings.Split(line, ":")
	if len(parts) < 2 {
		return "", 0, "", "", false
	}
	if reIPv4.MatchString(parts[0]) {
		if len(parts) == 2 {
			return finishPlainLine(parts[0], parts[1])
		}
		if len(parts) == 4 {
			return finishPlainLine(parts[0], strings.Join(parts[1:], ":"))
		}
		return "", 0, "", "", false
	}
	if len(parts) >= 2 {
		candidateIP := strings.Join(parts[:len(parts)-1], ":")
		if net.ParseIP(candidateIP) != nil {
			return finishPlainLine(candidateIP, parts[len(parts)-1])
		}
	}
	if len(parts) >= 4 {
		candidateIP := strings.Join(parts[:len(parts)-3], ":")
		if net.ParseIP(candidateIP) != nil {
			return finishPlainLine(candidateIP, strings.Join(parts[len(parts)-3:], ":"))
		}
	}
	return "", 0, "", "", false
}

func finishPlainLine(host, tail string) (string, int, string, string, bool) {
	fields := strings.SplitN(tail, ":", 3)
	port, perr := strconv.Atoi(fields[0])
	if perr != nil || port < 1 || port > 65535 {
		return "", 0, "", "", false
	}
	user, pass := "", ""
	if len(fields) == 3 {
		user, pass = fields[1], fields[2]
	} else if len(fields) == 2 {
		return "", 0, "", "", false // "PORT:USER" بدون پسورد فرمت پشتیبانی‌شده‌ای نیست
	}
	return host, port, user, pass, true
}

// plainLineToOutbound: خطوط ساده طبق مستندات Resin به‌عنوان پراکسی HTTP در نظر
// گرفته می‌شوند (نه SOCKS).
func plainLineToOutbound(host string, port int, user, pass string) map[string]interface{} {
	out := map[string]interface{}{
		"type": "http", "tag": fmt.Sprintf("%s:%d", host, port), "server": host, "server_port": port,
	}
	if user != "" || pass != "" {
		out["username"] = user
		out["password"] = pass
	}
	return out
}

// ---------------------------------------------------------------------
// منطق کسب‌وکار Subscriptions: fetch/refresh، namespaced‌کردن تگ‌ها، و ادغام با
// renderConfig (گروه auto اختصاصی هر subscription + عضویت در selectorOptions،
// دقیقاً هم‌الگو با WARP). فایل‌های خروجی subscriptionsDir/<name>.json — نه
// nodes.json و نه template.json — تنها منبع این نودها هستند.
// ---------------------------------------------------------------------

func subscriptionNodesPath(name string) string {
	return filepath.Join(subscriptionsDir, name+".json")
}

// existingWarpGroupPrefixes پیشوند گروه‌های WARP فعلی (از nodes.json) را
// برمی‌گرداند — برای جلوگیری از تداخل نام یک subscription با یک گروه WARP
// (هر دو تگ "<name>-auto" می‌سازند).
func existingWarpGroupPrefixes() map[string]bool {
	out := map[string]bool{}
	var nodes []interface{}
	_ = readJSON(nodesFile, &nodes)
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
		if prefix, grouped := groupPrefixForTag(tag); grouped {
			out[prefix] = true
		}
	}
	return out
}

var subReservedNames = map[string]bool{"auto": true, "direct": true}

func validateSubscriptionName(name string, state AppState) error {
	if !subscriptionNameRe.MatchString(name) {
		return fmt.Errorf("name must be 1-40 letters/digits/underscore/hyphen")
	}
	if subReservedNames[name] {
		return fmt.Errorf("%q is a reserved name", name)
	}
	if existingWarpGroupPrefixes()[name] {
		return fmt.Errorf("%q is already used as a WARP group name", name)
	}
	for _, s := range state.Subscriptions {
		if s.Name == name {
			return fmt.Errorf("a subscription named %q already exists", name)
		}
	}
	return nil
}

// namespaceSubscriptionOutbounds تگ هر outbound این subscription را به شکل
// "<subName>/<تگ اصلی>" یکتا می‌کند تا با تگ سایر subscriptionها، WARP، یا
// نودهای دستی برخورد نکند؛ تگ‌های تکراری داخل همین subscription هم با یک
// پسوند عددی جدا می‌شوند.
func namespaceSubscriptionOutbounds(subName string, obs []map[string]interface{}) []map[string]interface{} {
	seen := map[string]int{}
	out := make([]map[string]interface{}, 0, len(obs))
	for _, ob := range obs {
		clone := cloneMap(ob)
		origTag, _ := clone["tag"].(string)
		if origTag == "" {
			origTag = "node"
		}
		base := subName + "/" + origTag
		seen[base]++
		tag := base
		if n := seen[base]; n > 1 {
			tag = fmt.Sprintf("%s-%d", base, n)
		}
		clone["tag"] = tag
		out = append(out, clone)
	}
	return out
}

// fetchSubscriptionContent محتوای خام subscription را برمی‌گرداند: برای
// remote یک HTTP GET (با timeout و User-Agent سازگار با clash.meta تا
// ارائه‌دهنده‌هایی که بر اساس UA فیلتر می‌کنند هم جواب بدهند)، برای local همان
// Content ذخیره‌شده.
func fetchSubscriptionContent(sub Subscription) ([]byte, error) {
	if sub.SourceType == "local" {
		return []byte(sub.Content), nil
	}
	if sub.URL == "" {
		return nil, fmt.Errorf("no URL configured")
	}
	client := &http.Client{Timeout: 20 * time.Second}
	req, err := http.NewRequest("GET", sub.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "clash.meta/sing-box-manager")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20)) // سقف ۳۲ مگابایت، در برابر subscription بدخیم/بی‌نهایت محافظت می‌کند
	if err != nil {
		return nil, err
	}
	return body, nil
}

// refreshSubscription محتوا را می‌گیرد، پارس می‌کند، فایل کش را می‌نویسد، و
// وضعیت (node_count/last_fetched/last_error) را در state به‌روزرسانی و ذخیره
// می‌کند. چون رفرش فقط دستی است (بدون زمان‌بند پس‌زمینه)، این تابع مستقیماً از
// هندلرهای add/refresh صدا زده می‌شود.
func refreshSubscription(state *AppState, name string) error {
	idx := -1
	for i, s := range state.Subscriptions {
		if s.Name == name {
			idx = i
			break
		}
	}
	if idx == -1 {
		return fmt.Errorf("subscription %q not found", name)
	}
	sub := state.Subscriptions[idx]

	raw, fetchErr := fetchSubscriptionContent(sub)
	if fetchErr != nil {
		state.Subscriptions[idx].LastError = fetchErr.Error()
		state.Subscriptions[idx].LastFetched = time.Now().UTC().Format(time.RFC3339)
		_ = writeState(*state)
		return fetchErr
	}

	obs, warnings, parseErr := parseGeneralSubscription(raw)
	if parseErr != nil {
		state.Subscriptions[idx].LastError = parseErr.Error()
		state.Subscriptions[idx].LastFetched = time.Now().UTC().Format(time.RFC3339)
		_ = writeState(*state)
		return parseErr
	}

	if err := writeJSONAtomic(subscriptionNodesPath(name), map[string]interface{}{"outbounds": obs}); err != nil {
		state.Subscriptions[idx].LastError = "failed to write cache: " + err.Error()
		_ = writeState(*state)
		return err
	}

	lastErr := ""
	if len(warnings) > 0 {
		lastErr = fmt.Sprintf("%d node(s) skipped (see server log)", len(warnings))
		for _, w := range warnings {
			log.Printf("refreshSubscription %q: %s", name, w)
		}
	}
	state.Subscriptions[idx].NodeCount = len(obs)
	state.Subscriptions[idx].LastFetched = time.Now().UTC().Format(time.RFC3339)
	state.Subscriptions[idx].LastError = lastErr
	if err := writeState(*state); err != nil {
		return err
	}
	return nil
}

// loadSubscriptionOutbounds فایل‌های کش همه‌ی subscriptionهای فعال را می‌خواند
// و دقیقاً هم‌شکل خروجی حلقه‌ی WARP در renderConfig برمی‌گرداند — تا بشود آن‌ها
// را مستقیم به همان متغیرها append کرد و از همان منطق selectorOptions/fallback
// موجود (بدون تغییر) عبور دهد. subscription غیرفعال یا بدون کش معتبر بی‌صدا رد
// می‌شود؛ یک subscription خراب نباید کل تولید کانفیگ را متوقف کند.
func loadSubscriptionOutbounds(subs []Subscription, urltestDefaults map[string]interface{}) (endpointNodes []interface{}, proxyNodes []interface{}, groupAutoOutbounds []interface{}, groupAutoTags []string) {
	for _, sub := range subs {
		if !sub.Enabled {
			continue
		}
		var cache struct {
			Outbounds []map[string]interface{} `json:"outbounds"`
		}
		if err := readJSON(subscriptionNodesPath(sub.Name), &cache); err != nil || len(cache.Outbounds) == 0 {
			continue
		}
		namespaced := namespaceSubscriptionOutbounds(sub.Name, cache.Outbounds)
		var tags []string
		for _, ob := range namespaced {
			tag, _ := ob["tag"].(string)
			if tag == "" {
				continue
			}
			tags = append(tags, tag)
			typ, _ := ob["type"].(string)
			if typ == "wireguard" || typ == "tailscale" {
				endpointNodes = append(endpointNodes, interface{}(ob))
			} else {
				proxyNodes = append(proxyNodes, interface{}(ob))
			}
		}
		if len(tags) == 0 {
			continue
		}
		sort.Strings(tags)
		autoTag := sub.Name + "-auto"
		groupAutoTags = append(groupAutoTags, autoTag)
		groupAutoOutbounds = append(groupAutoOutbounds, map[string]interface{}{
			"tag": autoTag, "type": "urltest",
			"interval": urltestDefaults["interval"], "tolerance": urltestDefaults["tolerance"], "url": urltestDefaults["url"],
			"outbounds": toInterfaceSlice(tags),
		})
	}
	return
}

// ---------------------------------------------------------------------
// HTTP handlers: Subscriptions
// ---------------------------------------------------------------------

func listSubscriptionsHandler(w http.ResponseWriter, r *http.Request) {
	state := readStateOrDefault()
	if state.Subscriptions == nil {
		state.Subscriptions = []Subscription{}
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"subscriptions": state.Subscriptions})
}

func addSubscriptionHandler(w http.ResponseWriter, r *http.Request) {
	var req Subscription
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Invalid JSON"})
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.SourceType != "remote" && req.SourceType != "local" {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "source_type must be \"remote\" or \"local\""})
		return
	}
	if req.SourceType == "remote" && strings.TrimSpace(req.URL) == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "url is required for a remote subscription"})
		return
	}
	if req.SourceType == "local" && strings.TrimSpace(req.Content) == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "content is required for a local subscription"})
		return
	}

	state := readStateOrDefault()
	if err := validateSubscriptionName(req.Name, state); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": err.Error()})
		return
	}
	req.Enabled = true
	req.NodeCount = 0
	req.LastError = ""
	req.LastFetched = ""
	state.Subscriptions = append(state.Subscriptions, req)
	if err := writeState(state); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"error": "Failed to save: " + err.Error()})
		return
	}

	msg := fmt.Sprintf("Subscription %q added", req.Name)
	if err := refreshSubscription(&state, req.Name); err != nil {
		msg = fmt.Sprintf("Subscription %q added, but the first fetch failed: %v", req.Name, err)
	} else if err := applyCurrentTemplateAndNodes(); err != nil {
		msg = fmt.Sprintf("Subscription %q added and fetched, but applying it to sing-box failed: %v", req.Name, err)
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"message": msg})
}

func editSubscriptionHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name       string `json:"name"`
		SourceType string `json:"source_type,omitempty"`
		URL        string `json:"url,omitempty"`
		Content    string `json:"content,omitempty"`
		Enabled    *bool  `json:"enabled,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Invalid JSON"})
		return
	}
	state := readStateOrDefault()
	idx := -1
	for i, s := range state.Subscriptions {
		if s.Name == req.Name {
			idx = i
			break
		}
	}
	if idx == -1 {
		jsonResponse(w, http.StatusNotFound, map[string]interface{}{"error": fmt.Sprintf("subscription %q not found", req.Name)})
		return
	}
	if req.SourceType != "" {
		if req.SourceType != "remote" && req.SourceType != "local" {
			jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "source_type must be \"remote\" or \"local\""})
			return
		}
		state.Subscriptions[idx].SourceType = req.SourceType
	}
	if req.URL != "" {
		state.Subscriptions[idx].URL = req.URL
	}
	if req.Content != "" {
		state.Subscriptions[idx].Content = req.Content
	}
	if req.Enabled != nil {
		state.Subscriptions[idx].Enabled = *req.Enabled
	}
	if err := writeState(state); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"error": "Failed to save: " + err.Error()})
		return
	}
	msg := fmt.Sprintf("Subscription %q updated", req.Name)
	if state.Subscriptions[idx].Enabled {
		if err := refreshSubscription(&state, req.Name); err != nil {
			msg = fmt.Sprintf("Subscription %q updated, but refresh failed: %v", req.Name, err)
		} else if err := applyCurrentTemplateAndNodes(); err != nil {
			msg = fmt.Sprintf("Subscription %q updated and fetched, but applying it to sing-box failed: %v", req.Name, err)
		}
	} else if err := applyCurrentTemplateAndNodes(); err != nil {
		// غیرفعال شد — باید از selectorOptions زنده هم حذف شود.
		msg = fmt.Sprintf("Subscription %q disabled, but applying that to sing-box failed: %v", req.Name, err)
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"message": msg})
}

func refreshSubscriptionHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Invalid JSON"})
		return
	}
	state := readStateOrDefault()
	if err := refreshSubscription(&state, req.Name); err != nil {
		jsonResponse(w, http.StatusBadGateway, map[string]interface{}{"error": err.Error()})
		return
	}
	idx := -1
	for i, s := range state.Subscriptions {
		if s.Name == req.Name {
			idx = i
		}
	}
	resp := map[string]interface{}{
		"message":    fmt.Sprintf("Subscription %q refreshed", req.Name),
		"node_count": state.Subscriptions[idx].NodeCount,
	}
	if err := applyCurrentTemplateAndNodes(); err != nil {
		resp["message"] = fmt.Sprintf("Subscription %q refreshed, but applying it to sing-box failed: %v", req.Name, err)
	}
	jsonResponse(w, http.StatusOK, resp)
}

func deleteSubscriptionHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Invalid JSON"})
		return
	}
	state := readStateOrDefault()
	var kept []Subscription
	found := false
	for _, s := range state.Subscriptions {
		if s.Name == req.Name {
			found = true
			continue
		}
		kept = append(kept, s)
	}
	if !found {
		jsonResponse(w, http.StatusNotFound, map[string]interface{}{"error": fmt.Sprintf("subscription %q not found", req.Name)})
		return
	}
	state.Subscriptions = kept
	if err := writeState(state); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"error": "Failed to save: " + err.Error()})
		return
	}
	_ = os.Remove(subscriptionNodesPath(req.Name))
	msg := fmt.Sprintf("Subscription %q removed", req.Name)
	if err := applyCurrentTemplateAndNodes(); err != nil {
		msg = fmt.Sprintf("Subscription %q removed, but applying that to sing-box failed: %v", req.Name, err)
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"message": msg})
}

// ---------------------------------------------------------------------
// Reverse proxy عمومی (پورت proxyPort، پیش‌فرض 80): تنها ورودی عمومی برنامه.
// هر DockerService زیر پیشوند "/" + Name سرو می‌شود؛ هر چیز دیگری (شامل
// خودِ پنل مدیریت روی "/") به apiPort محلی فوروارد می‌شود. این یعنی چه در
// حالت quick tunnel (که فقط یک مقصد دارد)، چه tunnel_token، چه api_token،
// یک هاست‌نیم/تونل واحد کل برنامه را سرو می‌کند — دیگر نیازی به ingress یا
// DNS جدا به‌ازای هر سرویس نیست.
// ---------------------------------------------------------------------

// currentProxyMux به‌صورت atomic نگه‌داری می‌شود تا rebuild (بعد از هر
// add/delete DockerService) بدون قفل‌گرفتن روی مسیر request و بدون downtime
// انجام شود.
var currentProxyMux atomic.Pointer[http.ServeMux]

// buildProxyMux جدول مسیر عمومی پورت proxyPort را از روی DockerServiceهای
// فعلی می‌سازد. هر سرویس با http.StripPrefix پیشوند خودش را از مسیر حذف
// می‌کند تا upstream دقیقاً همان مسیری را ببیند که برای خودش انتظار دارد
// (مثلاً "/xai/v1/models" → upstream فقط "/v1/models" را می‌بیند).
func buildProxyMux(services []ServiceDef) *http.ServeMux {
	mux := http.NewServeMux()

	for _, svc := range services {
		target, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", svc.ListenPort))
		if err != nil {
			log.Printf("buildProxyMux: skipping %q: %v", svc.Name, err)
			continue
		}
		name := svc.Name
		prefix := "/" + name
		proxy := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(target)
				pr.Out.Host = target.Host
				pr.Out.Header.Set("X-Forwarded-Prefix", prefix)
			},
			ModifyResponse: func(res *http.Response) error {
				if loc := res.Header.Get("Location"); loc != "" {
					if u, err := url.Parse(loc); err == nil && u.Host == "" && strings.HasPrefix(u.Path, "/") {
						u.Path = prefix + u.Path
						res.Header.Set("Location", u.String())
					}
				}
				return nil
			},
			// FlushInterval=-1 یعنی هر write بلافاصله flush می‌شود — بدون این،
			// پاسخ‌های stream=true (SSE، مثل OpenAI/Anthropic-style APIها) بافر و
			// تاخیردار به کلاینت می‌رسند.
			FlushInterval: -1,
			ErrorLog:      log.Default(),
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				log.Printf("reverse proxy: /%s -> %s failed: %v", name, target, err)
				w.WriteHeader(http.StatusBadGateway)
				_, _ = fmt.Fprintf(w, "Bad Gateway: Service %q on 127.0.0.1:%d is unreachable (%v)\n", name, svc.ListenPort, err)
			},
		}
		stripped := http.StripPrefix(prefix, proxy)
		mux.Handle(prefix+"/", stripped)
		// درخواست بدون "/" انتهایی را هم پاسخ بده — کلاینت‌های OpenAI/Anthropic-style
		// base_url را گاهی بدون trailing slash می‌سازند.
		mux.HandleFunc(prefix, func(w http.ResponseWriter, r *http.Request) {
			r2 := r.Clone(r.Context())
			r2.URL.Path = prefix + "/"
			stripped.ServeHTTP(w, r2)
		})

		// برای داشبوردهایی مثل metacubexd که مستقیماً به روت درخواست می‌دهند
		if name == "singbox" {
			clashEndpoints := []string{"/proxies", "/configs", "/logs", "/traffic", "/connections", "/providers", "/rules", "/version"}
			for _, p := range clashEndpoints {
				mux.Handle(p, proxy)
				mux.Handle(p+"/", proxy)
			}
		}
	}

	// هر مسیر دیگری (شامل ریشه‌ی "/") به پنل مدیریت خودِ همین برنامه می‌رود —
	// دقیقاً همان چیزی که روی bindAddr:apiPort در حال اجراست.
	panelTarget, _ := url.Parse("http://127.0.0.1" + apiPort)
	panelProxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) { pr.SetURL(panelTarget) },
	}
	mux.Handle("/", panelProxy)

	return mux
}

// rebuildProxyRoutes باید بعد از هر add/delete DockerService و در startup
// صدا زده شود.
func rebuildProxyRoutes() {
	state := readStateOrDefault()
	services := state.Services

	// Add singbox API automatically
	if clashAddr, err := getClashAPIAddr(); err == nil {
		parts := strings.Split(clashAddr, ":")
		if len(parts) == 2 {
			if port, err := strconv.Atoi(parts[1]); err == nil {
				services = append(services, ServiceDef{
					Name:       "singbox",
					ListenPort: port,
				})
			}
		}
	}

	currentProxyMux.Store(buildProxyMux(services))
}

func proxyRouterHandler(w http.ResponseWriter, r *http.Request) {
	mux := currentProxyMux.Load()
	if mux == nil {
		http.Error(w, "starting up", http.StatusServiceUnavailable)
		return
	}
	mux.ServeHTTP(w, r)
}

// reverseProxyServer نگه‌داری می‌شود تا در shutdown برنامه بتوان آن را هم
// graceful خاموش کرد.
var reverseProxyServer *http.Server

// startReverseProxyServer تنها ورودی عمومی برنامه را روی 0.0.0.0:proxyPort
// (برخلاف apiPort که پیش‌فرض فقط 127.0.0.1 است) بالا می‌آورد — چون هم از
// طریق تونل و هم (در صورت دسترسی مستقیم) باید از بیرون قابل‌رسیدن باشد.
func startReverseProxyServer() {
	rebuildProxyRoutes()
	reverseProxyServer = &http.Server{
		Addr:    "0.0.0.0:" + proxyPort,
		Handler: http.HandlerFunc(proxyRouterHandler),
	}
	go func() {
		log.Printf("🌐 Reverse proxy listening on :%s (docker services under /<name>/, everything else → panel)", proxyPort)
		if err := reverseProxyServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("reverse proxy server stopped: %v", err)
		}
	}()
}

// ---------------------------------------------------------------------
// وب‌سرویس‌های جدا (Docker): هر کدام زیر پیشوند مسیر "/" + Name روی reverse
// proxy محلی (بالا) سرو می‌شوند. اینها مستقل از جدول Services (پروکسی‌های
// sing-box) هستند.
// ---------------------------------------------------------------------

// reservedServiceNames نام‌هایی هستند که نمی‌توان به‌عنوان DockerService ثبت
// کرد چون با مسیرهای خودِ پنل مدیریت روی reverse proxy تداخل پیدا می‌کنند.
var reservedServiceNames = map[string]bool{
	"api": true, "admin": true, "static": true, "assets": true, "favicon.ico": true,
}

// Old docker web apps handlers removed to merge into Services.

// settingsHandler وضعیت کلی تنظیمات قابل‌مدیریت از UI را برمی‌گرداند.
func settingsHandler(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"admin_token_set": getAdminToken() != "",
		"bind_addr":       bindAddr,
		"api_port":        apiPort,
		"proxy_port":      proxyPort,
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
		defaultVal := managedEnvDefaults[key]
		effective := getSetting(key, defaultVal)
		_, overridden := state.EnvOverrides[key]
		out[key] = map[string]interface{}{
			"value":             effective,
			"default_value":     defaultVal,
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
	// serviceHosts نگاشت "کلید منطقی" (panel/apps/dash) به هاست‌نیم عمومی واقعی
	// است. "apps" هاست‌نیمی است که reverse proxy محلی (و همه‌ی DockerServiceها
	// زیر پیشوند مسیرشان) رویش سرو می‌شود. فرانت‌اند برای ساختن URL جدول
	// Docker services از همین استفاده می‌کند، نه با حدس زدن window.location.
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

	configs, err := GenerateWireGuardConfigs(req.Tag, account, []string{getGlobalWarpEndpoint()})
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

func applyWarpEndpointToNodes(ep string) error {
	mu.Lock()
	defer mu.Unlock()

	var nodesRaw []interface{}
	if err := readJSON(nodesFile, &nodesRaw); err != nil && !os.IsNotExist(err) {
		return err
	}

	host, port, _ := parseEndpoint(ep)

	for _, n := range nodesRaw {
		if m, ok := n.(map[string]interface{}); ok {
			if t, ok := m["type"].(string); ok && t == "wireguard" {
				delete(m, "server")
				delete(m, "server_port")

				if peers, ok := m["peers"].([]interface{}); ok && len(peers) > 0 {
					if peerMap, ok := peers[0].(map[string]interface{}); ok {
						peerMap["address"] = host
						peerMap["port"] = port
					}
				}
			}
		}
	}

	b, _ := json.MarshalIndent(nodesRaw, "", "  ")
	_ = atomicWriteFile(nodesFile, b, 0644)

	// Apply change to sing-box
	var tmpl map[string]interface{}
	if err := readJSON(templateFile, &tmpl); err != nil {
		return err
	}
	return applyChangeFromStruct(tmpl, nodesRaw)
}

func getWarpEndpointHandler(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"endpoint": getGlobalWarpEndpoint(),
	})
}

func pingCurrentWarpEndpointHandler(w http.ResponseWriter, r *http.Request) {
	ep := getGlobalWarpEndpoint()
	results, _, err := scanBatchWarpEndpoints(1, []string{}, "both")
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"error": err.Error()})
		return
	}

	var curItem *WarpScanItem
	for _, res := range results {
		if res.Endpoint == ep {
			curItem = &res
			break
		}
	}

	if curItem == nil {
		mu.Lock()
		var nodes []map[string]interface{}
		_ = readJSON(nodesFile, &nodes)
		mu.Unlock()

		var sampleWarp map[string]interface{}
		for _, n := range nodes {
			if t, ok := n["type"].(string); ok && t == "wireguard" {
				sampleWarp = n
				break
			}
		}

		if sampleWarp != nil {
			host, port, _ := parseEndpoint(ep)
			peer := map[string]interface{}{"address": host, "port": port}
			if peers, ok := sampleWarp["peers"].([]interface{}); ok && len(peers) > 0 {
				if p, ok := peers[0].(map[string]interface{}); ok {
					if val, ok := p["public_key"]; ok { peer["public_key"] = val }
					if val, ok := p["allowed_ips"]; ok { peer["allowed_ips"] = val }
					if val, ok := p["reserved"]; ok { peer["reserved"] = val }
				}
			}
			ob := map[string]interface{}{
				"type": "wireguard", "tag": "WARP_CUR_TEST", "peers": []interface{}{peer},
			}
			if val, ok := sampleWarp["address"]; ok { ob["address"] = val }
			if val, ok := sampleWarp["private_key"]; ok { ob["private_key"] = val }
			if val, ok := sampleWarp["mtu"]; ok { ob["mtu"] = val }

			cfg := map[string]interface{}{
				"endpoints": []interface{}{ob},
				"outbounds": []map[string]interface{}{
					{"type": "urltest", "tag": "test_urltest", "outbounds": []string{"WARP_CUR_TEST"}, "url": "http://cp.cloudflare.com/generate_204", "interval": "3m"},
				},
				"experimental": map[string]interface{}{
					"clash_api": map[string]interface{}{"external_controller": "127.0.0.1:9096"},
				},
			}
			tmpFile := filepath.Join(filepath.Dir(nodesFile), "temp_warp_cur_ping.json")
			b, _ := json.MarshalIndent(cfg, "", "  ")
			_ = os.WriteFile(tmpFile, b, 0644)
			defer os.Remove(tmpFile)

			singboxBin, _ := findSingBox()
			if singboxBin != "" {
				cmd := exec.Command(singboxBin, "run", "-c", tmpFile)
				if err := cmd.Start(); err == nil {
					defer func() {
						if cmd.Process != nil { cmd.Process.Kill() }
					}()
					time.Sleep(2 * time.Second)
					testReqURL := "http://127.0.0.1:9096/proxies/test_urltest/delay?timeout=3000&url=http://cp.cloudflare.com/generate_204"
					if testResp, err := http.Get(testReqURL); err == nil { testResp.Body.Close() }
					if resp, err := http.Get("http://127.0.0.1:9096/proxies"); err == nil {
						var apiResp struct {
							Proxies map[string]struct {
								History []struct { Delay int `json:"delay"` } `json:"history"`
							} `json:"proxies"`
						}
						_ = json.NewDecoder(resp.Body).Decode(&apiResp)
						resp.Body.Close()
						if p, ok := apiResp.Proxies["WARP_CUR_TEST"]; ok && len(p.History) > 0 {
							if p.History[len(p.History)-1].Delay > 0 {
								curItem = &WarpScanItem{
									Endpoint: ep,
									Delay: p.History[len(p.History)-1].Delay,
									IsDefault: ep == "engage.cloudflareclient.com:2408",
								}
							}
						}
					}
				}
			}
		}
	}

	delay := -1
	if curItem != nil {
		delay = curItem.Delay
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"endpoint": ep,
		"delay":    delay,
	})
}

func getWarpBackupsHandler(w http.ResponseWriter, r *http.Request) {
	state := readStateOrDefault()
	currentEP := getGlobalWarpEndpoint()

	seen := make(map[string]bool)
	var list []string

	addEP := func(ep string) {
		if ep != "" && !seen[ep] {
			seen[ep] = true
			list = append(list, ep)
		}
	}

	addEP("engage.cloudflareclient.com:2408")
	addEP(currentEP)
	for _, b := range state.BackupWarpEndpoints {
		addEP(b)
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"current": currentEP,
		"backups": list,
	})
}

// testWarpBackupsHandler pings the backups and returns delays
func testWarpBackupsHandler(w http.ResponseWriter, r *http.Request) {
	results, _, err := scanBatchWarpEndpoints(0, []string{}, "both")
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"results": results,
	})
}

// scanWarpBatchHandler handles batch scanning request

func applyWarpEndpointHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Endpoint string `json:"endpoint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Endpoint) == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Invalid endpoint"})
		return
	}

	ep := strings.TrimSpace(req.Endpoint)
	state := readStateOrDefault()
	state.GlobalWarpEndpoint = ep
	_ = writeState(state)

	if err := applyWarpEndpointToNodes(ep); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"error": "Failed to apply endpoint: " + err.Error()})
		return
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"message":  "Applied global WARP endpoint: " + ep,
		"endpoint": ep,
	})
}

func resetWarpEndpointHandler(w http.ResponseWriter, r *http.Request) {
	state := readStateOrDefault()
	state.GlobalWarpEndpoint = ""
	_ = writeState(state)

	ep := getGlobalWarpEndpoint()
	if err := applyWarpEndpointToNodes(ep); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"error": "Failed to apply default endpoint: " + err.Error()})
		return
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"message":  "Reset to default WARP endpoint",
		"endpoint": ep,
	})
}
// ---------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------
// =====================================================================
// Backup / Restore  (S3-سازگار عمومی، Cloudflare R2، Telegram Bot)
// =====================================================================
//
// همه‌ی پیکربندی از طریق getSetting/setSetting (managedEnvKeys) ذخیره
// می‌شود — یعنی هم از UI و هم با متغیر محیطی زمان استارت کانتینر قابل
// تنظیم است (دومی برای رفع مشکل مرغ‌وتخم‌مرغِ بازیابی در init لازم است).
//
// ایندکس محلی (backup-index.json) داخل خودِ /data نگه داشته می‌شود، چون
// /data همیشه یکی از مسیرهای بکاپ است: یعنی خودِ ایندکس هم در هر بکاپ بعدی
// بکاپ می‌شود و با بازیابی، خودش هم برمی‌گردد — بدون اینکه لازم باشد از سه
// مقصد مختلف (که تلگرام اصلاً API لیست‌کردن ندارد) لیست را از نو بسازیم.

const backupIndexFile = "/data/.backup-index.json"

// backupIndexEntry یک آرشیو بکاپ آپلودشده را توصیف می‌کند.
type backupIndexEntry struct {
	Cadence        string   `json:"cadence"`     // hourly | daily | monthly | manual
	Destination    string   `json:"destination"` // s3 | cloudflare_r2 | telegram
	Key            string   `json:"key"`         // s3/r2: object key آرشیو. telegram: comma-joined file_id قطعات به ترتیب
	ManifestKey    string   `json:"manifest_key,omitempty"`
	TelegramMsgIDs []string `json:"telegram_msg_ids,omitempty"` // فقط تلگرام، برای حذف موقع prune
	Timestamp      string   `json:"timestamp"`                  // RFC3339 (UTC)
	SizeBytes      int64    `json:"size_bytes"`
	Encrypted      bool     `json:"encrypted"`
	Paths          []string `json:"paths"`
}

type backupIndex struct {
	Entries []backupIndexEntry `json:"entries"`
}

func backupLoadIndex() backupIndex {
	var idx backupIndex
	data, err := os.ReadFile(backupIndexFile)
	if err != nil {
		return idx
	}
	_ = json.Unmarshal(data, &idx)
	return idx
}

func backupSaveIndex(idx backupIndex) error {
	if err := os.MkdirAll(filepath.Dir(backupIndexFile), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(backupIndexFile, data, 0600)
}

func backupAppendIndexEntry(e backupIndexEntry) error {
	backupIndexMu.Lock()
	defer backupIndexMu.Unlock()
	idx := backupLoadIndex()
	idx.Entries = append(idx.Entries, e)
	return backupSaveIndex(idx)
}

var backupIndexMu sync.Mutex
var backupRunMu sync.Mutex // یک بکاپ در آنِ واحد (دستی یا زمان‌بندی‌شده هم‌پوشانی نکنند)

// ---------------------------------------------------------------------
// تنظیمات
// ---------------------------------------------------------------------

// backupDefaultPaths مسیرهایی که همیشه (بدون امکان حذف) بکاپ/بازیابی می‌شوند.
func backupDefaultPaths() []string { return []string{"/data", "/app/data"} }

// backupExtraPaths مسیرهای اضافه‌ای که کاربر خودش از Settings اضافه کرده.
func backupExtraPaths() []string {
	raw := strings.TrimSpace(getSetting("BACKUP_PATHS_EXTRA", ""))
	if raw == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func backupAllPaths() []string {
	return append(append([]string{}, backupDefaultPaths()...), backupExtraPaths()...)
}

func backupSetExtraPaths(paths []string) error {
	return setSetting("BACKUP_PATHS_EXTRA", strings.Join(paths, ","))
}

func backupIntSetting(key string, def int) int {
	v := strings.TrimSpace(getSetting(key, ""))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	return n
}

func backupBoolSetting(key string, def bool) bool {
	v := strings.TrimSpace(getSetting(key, ""))
	if v == "" {
		return def
	}
	return strings.EqualFold(v, "true") || v == "1"
}

// backupCadenceConfig تنظیمات یک بازه‌ی زمان‌بندی (ساعتی/روزانه/ماهانه) را برمی‌گرداند.
type backupCadenceConfig struct {
	Name    string
	Enabled bool
	Keep    int
	Period  time.Duration // برای تشخیص "الان وقتشه یا نه" در scheduler
}

func backupCadences() []backupCadenceConfig {
	return []backupCadenceConfig{
		{Name: "hourly", Enabled: backupBoolSetting("BACKUP_HOURLY_ENABLED", false), Keep: backupIntSetting("BACKUP_HOURLY_KEEP", 24), Period: time.Hour},
		{Name: "daily", Enabled: backupBoolSetting("BACKUP_DAILY_ENABLED", false), Keep: backupIntSetting("BACKUP_DAILY_KEEP", 7), Period: 24 * time.Hour},
		{Name: "monthly", Enabled: backupBoolSetting("BACKUP_MONTHLY_ENABLED", false), Keep: backupIntSetting("BACKUP_MONTHLY_KEEP", 6), Period: 30 * 24 * time.Hour},
	}
}

func backupAnyDestinationEnabled() bool {
	_, s3ok := backupS3Target()
	_, r2ok := backupR2Target()
	tgOK := backupBoolSetting("BACKUP_TELEGRAM_ENABLED", false) &&
		getSetting("BACKUP_TELEGRAM_BOT_TOKEN", "") != "" && getSetting("BACKUP_TELEGRAM_CHAT_ID", "") != ""
	return s3ok || r2ok || tgOK
}

// ---------------------------------------------------------------------
// آرشیو‌سازی + رمزگذاری اختیاری (AES-256-CTR استریمی — برای محرمانگی؛ برای
// یکپارچگی/tamper-detection به‌جای این باید از حجم عملیاتی GCM با
// فریم‌بندی chunk استفاده می‌شد که برای این حجم کد صرف نشد)
// ---------------------------------------------------------------------

func backupArchiveBaseName(cadence string) string {
	return fmt.Sprintf("backup-%s-%s", cadence, time.Now().UTC().Format("20060102-150405"))
}

// backupCreateTarGz مسیرهای داده‌شده را با مسیر مطلق کامل (بدون "/" ابتدایی)
// داخل tar.gz می‌ریزد تا موقع extract دقیقاً به همان مسیر مطلق برگردند و
// تداخلی بین "/data" و "/app/data" پیش نیاید.
func backupCreateTarGz(destPath string, paths []string) error {
	f, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	for _, root := range paths {
		root = filepath.Clean(root)
		if _, err := os.Lstat(root); err != nil {
			if os.IsNotExist(err) {
				continue // مسیر پیکربندی‌شده ولی هنوز روی دیسک نیست، رد شو
			}
			return err
		}
		werr := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			// خودِ فایل ایندکس را دوباره داخل خودش نریز (بی‌ضرره ولی لازم نیست)
			if p == backupIndexFile {
				return nil
			}
			name := strings.TrimPrefix(p, "/")
			if fi.IsDir() {
				if name != "" {
					hdr, _ := tar.FileInfoHeader(fi, "")
					hdr.Name = name + "/"
					_ = tw.WriteHeader(hdr)
				}
				return nil
			}
			if !fi.Mode().IsRegular() {
				return nil // سوکت/دیوایس/سیملینک عجیب را رد کن
			}
			hdr, err := tar.FileInfoHeader(fi, "")
			if err != nil {
				return err
			}
			hdr.Name = name
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			rf, err := os.Open(p)
			if err != nil {
				return err
			}
			defer rf.Close()
			_, err = io.Copy(tw, rf)
			return err
		})
		if werr != nil {
			return werr
		}
	}
	return nil
}

func backupExtractTarGz(srcPath string) error {
	f, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		target := "/" + strings.TrimPrefix(hdr.Name, "/")
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			mode := os.FileMode(hdr.Mode)
			if mode == 0 {
				mode = 0644
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			out.Close()
		}
	}
	return nil
}

// backupEncryptFile با AES-256-CTR رمزگذاری استریمی می‌کند (حافظه‌ی ثابت،
// برای آرشیوهای بزرگ مناسب است). IV تصادفی ۱۶ بایتی اول فایل رمزشده ذخیره
// می‌شود. نکته: CTR بدون تگ احراز اصالت است — یعنی محرمانگی تضمین می‌شود
// اما دستکاری آرشیو رمزشده (tamper) تشخیص داده نمی‌شود.
func backupEncryptFile(passphrase, srcPath, dstPath string) error {
	key := sha256.Sum256([]byte(passphrase))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return err
	}
	iv := make([]byte, aes.BlockSize)
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return err
	}
	in, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := out.Write(iv); err != nil {
		return err
	}
	stream := cipher.NewCTR(block, iv)
	writer := &cipher.StreamWriter{S: stream, W: out}
	_, err = io.Copy(writer, in)
	return err
}

func backupDecryptFile(passphrase, srcPath, dstPath string) error {
	key := sha256.Sum256([]byte(passphrase))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return err
	}
	in, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer in.Close()
	iv := make([]byte, aes.BlockSize)
	if _, err := io.ReadFull(in, iv); err != nil {
		return fmt.Errorf("archive too short or not encrypted: %w", err)
	}
	out, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer out.Close()
	stream := cipher.NewCTR(block, iv)
	reader := &cipher.StreamReader{S: stream, R: in}
	_, err = io.Copy(out, reader)
	return err
}

// ---------------------------------------------------------------------
// مقصد S3-سازگار عمومی + Cloudflare R2 (هر دو با همان کلاینت minio-go)
// ---------------------------------------------------------------------

type backupS3TargetCfg struct {
	label     string
	endpoint  string
	region    string
	bucket    string
	accessKey string
	secretKey string
	prefix    string
	useSSL    bool
}

func (t backupS3TargetCfg) objectKey(key string) string {
	if t.prefix == "" {
		return key
	}
	return t.prefix + "/" + key
}

func backupS3Target() (backupS3TargetCfg, bool) {
	if !backupBoolSetting("BACKUP_S3_ENABLED", false) {
		return backupS3TargetCfg{}, false
	}
	ep := strings.TrimSpace(getSetting("BACKUP_S3_ENDPOINT", "s3.amazonaws.com"))
	ep = strings.TrimPrefix(strings.TrimPrefix(ep, "https://"), "http://")
	bucket := getSetting("BACKUP_S3_BUCKET", "")
	accessKey := getSetting("BACKUP_S3_ACCESS_KEY", "")
	if ep == "" || bucket == "" || accessKey == "" {
		return backupS3TargetCfg{}, false
	}
	return backupS3TargetCfg{
		label:     "s3",
		endpoint:  ep,
		region:    getSetting("BACKUP_S3_REGION", "us-east-1"),
		bucket:    bucket,
		accessKey: accessKey,
		secretKey: getSetting("BACKUP_S3_SECRET_KEY", ""),
		prefix:    strings.Trim(getSetting("BACKUP_S3_PREFIX", ""), "/"),
		useSSL:    backupBoolSetting("BACKUP_S3_USE_SSL", true),
	}, true
}

// backupResolveCFAccount توکن/accountID لازم برای R2 را برمی‌گرداند: اول از
// state.json کش‌شده (اگر تونل api_token قبلاً راه‌اندازی شده)، وگرنه دوباره
// از zone_name resolve می‌کند.
func backupResolveCFAccount() (token, accountID string, err error) {
	cfg := readStateOrDefault().Cloudflare
	token = strings.TrimSpace(cfg.APIToken)
	if token == "" {
		return "", "", fmt.Errorf("Cloudflare API Token تنظیم نشده (تنظیمات → Cloudflare Tunnel)")
	}
	if cfg.AccountID != "" {
		return token, cfg.AccountID, nil
	}
	if cfg.ZoneName == "" {
		return "", "", fmt.Errorf("دامنه/Zone در تنظیمات Cloudflare مشخص نشده")
	}
	_, accountID, err = cfResolveZone(token, cfg.ZoneName)
	return token, accountID, err
}

func backupR2Target() (backupS3TargetCfg, bool) {
	if !backupBoolSetting("BACKUP_R2_ENABLED", false) {
		return backupS3TargetCfg{}, false
	}
	bucket := getSetting("BACKUP_R2_BUCKET", "")
	accessKey := getSetting("BACKUP_R2_ACCESS_KEY", "")
	if bucket == "" || accessKey == "" {
		return backupS3TargetCfg{}, false
	}
	accountID := readStateOrDefault().Cloudflare.AccountID
	if accountID == "" {
		if _, resolved, err := backupResolveCFAccount(); err == nil {
			accountID = resolved
		}
	}
	if accountID == "" {
		return backupS3TargetCfg{}, false
	}
	return backupS3TargetCfg{
		label:     "cloudflare_r2",
		endpoint:  accountID + ".r2.cloudflarestorage.com",
		region:    "auto",
		bucket:    bucket,
		accessKey: accessKey,
		secretKey: getSetting("BACKUP_R2_SECRET_KEY", ""),
		prefix:    strings.Trim(getSetting("BACKUP_R2_PREFIX", ""), "/"),
		useSSL:    true,
	}, true
}

func backupS3Client(t backupS3TargetCfg) (*minio.Client, error) {
	return minio.New(t.endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(t.accessKey, t.secretKey, ""),
		Secure: t.useSSL,
		Region: t.region,
	})
}

func backupS3Upload(t backupS3TargetCfg, key, filePath string) error {
	cli, err := backupS3Client(t)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	_, err = cli.FPutObject(ctx, t.bucket, t.objectKey(key), filePath, minio.PutObjectOptions{})
	return err
}

func backupS3Download(t backupS3TargetCfg, key, destPath string) error {
	cli, err := backupS3Client(t)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	return cli.FGetObject(ctx, t.bucket, t.objectKey(key), destPath, minio.GetObjectOptions{})
}

func backupS3Delete(t backupS3TargetCfg, key string) error {
	cli, err := backupS3Client(t)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return cli.RemoveObject(ctx, t.bucket, t.objectKey(key), minio.RemoveObjectOptions{})
}

// ---------------------------------------------------------------------
// مقصد Telegram Bot
// ---------------------------------------------------------------------

// telegramChunkSize کمی زیر سقف ۲۰ مگابایتیِ getFile نگه داشته می‌شود، چون
// محدودیت واقعیِ رفت‌وبرگشت (آپلود ۵۰ مگابایت ولی دانلود فقط ۲۰ مگابایت
// از طریق Bot API استاندارد) همان ۲۰ مگابایت است — بدون این، بازیابی از
// روی قطعات بزرگ‌تر اصلاً ممکن نیست.
const telegramChunkSize = 18 * 1024 * 1024

func telegramSendDocument(botToken, chatID, filename string, content io.Reader, caption string) (fileID, messageID string, err error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("chat_id", chatID)
	if caption != "" {
		_ = mw.WriteField("caption", caption)
	}
	part, err := mw.CreateFormFile("document", filename)
	if err != nil {
		return "", "", err
	}
	if _, err := io.Copy(part, content); err != nil {
		return "", "", err
	}
	if err := mw.Close(); err != nil {
		return "", "", err
	}
	req, err := http.NewRequest(http.MethodPost, "https://api.telegram.org/bot"+botToken+"/sendDocument", &buf)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	var parsed struct {
		Ok     bool `json:"ok"`
		Result struct {
			MessageID int `json:"message_id"`
			Document  struct {
				FileID string `json:"file_id"`
			} `json:"document"`
		} `json:"result"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", "", err
	}
	if !parsed.Ok {
		return "", "", fmt.Errorf("Telegram API error: %s", parsed.Description)
	}
	return parsed.Result.Document.FileID, strconv.Itoa(parsed.Result.MessageID), nil
}

func telegramDeleteMessage(botToken, chatID, messageID string) error {
	req, err := http.NewRequest(http.MethodPost, "https://api.telegram.org/bot"+botToken+"/deleteMessage",
		strings.NewReader(url.Values{"chat_id": {chatID}, "message_id": {messageID}}.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil // بهترین‌تلاش؛ اگر پیام از قبل پاک شده بود هم مهم نیست
}

func telegramDownloadFile(botToken, fileID, destPath string) error {
	getFileURL := "https://api.telegram.org/bot" + botToken + "/getFile?file_id=" + url.QueryEscape(fileID)
	resp, err := http.Get(getFileURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var parsed struct {
		Ok     bool `json:"ok"`
		Result struct {
			FilePath string `json:"file_path"`
		} `json:"result"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return err
	}
	if !parsed.Ok {
		return fmt.Errorf("Telegram getFile error: %s", parsed.Description)
	}
	dresp, err := http.Get("https://api.telegram.org/file/bot" + botToken + "/" + parsed.Result.FilePath)
	if err != nil {
		return err
	}
	defer dresp.Body.Close()
	out, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, dresp.Body)
	return err
}

// telegramUploadFile فایل را در صورت نیاز به قطعات ≤۱۸ مگابایتی می‌شکند و
// هر قطعه را جدا می‌فرستد؛ file_id هر قطعه (به ترتیب) برای بازسازی لازم است.
func telegramUploadFile(botToken, chatID, filePath, caption string) (fileIDs, msgIDs []string, err error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	base := filepath.Base(filePath)
	if fi.Size() <= telegramChunkSize {
		fid, mid, err := telegramSendDocument(botToken, chatID, base, f, caption)
		if err != nil {
			return nil, nil, err
		}
		return []string{fid}, []string{mid}, nil
	}
	buf := make([]byte, telegramChunkSize)
	part := 0
	for {
		n, rerr := io.ReadFull(f, buf)
		if n > 0 {
			part++
			name := fmt.Sprintf("%s.part%03d", base, part)
			fid, mid, err := telegramSendDocument(botToken, chatID, name, bytes.NewReader(buf[:n]), fmt.Sprintf("%s (part %d)", caption, part))
			if err != nil {
				return fileIDs, msgIDs, err
			}
			fileIDs = append(fileIDs, fid)
			msgIDs = append(msgIDs, mid)
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return fileIDs, msgIDs, rerr
		}
	}
	return fileIDs, msgIDs, nil
}

func telegramDownloadParts(botToken string, fileIDs []string, destPath string) error {
	out, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer out.Close()
	tmp, err := os.MkdirTemp("", "tg-part-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	for i, fid := range fileIDs {
		partPath := filepath.Join(tmp, fmt.Sprintf("part-%03d", i))
		if err := telegramDownloadFile(botToken, fid, partPath); err != nil {
			return fmt.Errorf("part %d/%d: %w", i+1, len(fileIDs), err)
		}
		pf, err := os.Open(partPath)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, pf)
		pf.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------
// اجرای بکاپ (همه‌ی مقصدهای فعال) + prune + بازیابی
// ---------------------------------------------------------------------

// runBackupNow یک آرشیو تازه می‌سازد و به تمام مقصدهای فعال آپلود می‌کند.
// cadence یکی از hourly/daily/monthly/manual است (فقط برای اسم‌گذاری/نگهداری).
func runBackupNow(cadence string) (results map[string]string, err error) {
	backupRunMu.Lock()
	defer backupRunMu.Unlock()

	results = map[string]string{}
	paths := backupAllPaths()

	tmpDir, err := os.MkdirTemp("", "singbox-backup-")
	if err != nil {
		return results, err
	}
	defer os.RemoveAll(tmpDir)

	base := backupArchiveBaseName(cadence)
	rawPath := filepath.Join(tmpDir, base+".tar.gz")
	if err := backupCreateTarGz(rawPath, paths); err != nil {
		return results, fmt.Errorf("failed to build archive: %w", err)
	}

	uploadPath := rawPath
	archiveName := base + ".tar.gz"
	encrypted := false
	if pass := getSetting("BACKUP_PASSPHRASE", ""); pass != "" {
		encPath := filepath.Join(tmpDir, base+".tar.gz.enc")
		if err := backupEncryptFile(pass, rawPath, encPath); err != nil {
			return results, fmt.Errorf("failed to encrypt archive: %w", err)
		}
		uploadPath = encPath
		archiveName = base + ".tar.gz.enc"
		encrypted = true
	}

	fi, statErr := os.Stat(uploadPath)
	var size int64
	if statErr == nil {
		size = fi.Size()
	}
	manifest := map[string]interface{}{
		"cadence": cadence, "paths": paths, "encrypted": encrypted,
		"timestamp": time.Now().UTC().Format(time.RFC3339), "size_bytes": size,
	}
	manifestPath := filepath.Join(tmpDir, base+".manifest.json")
	if mb, merr := json.MarshalIndent(manifest, "", "  "); merr == nil {
		_ = os.WriteFile(manifestPath, mb, 0644)
	}
	manifestName := base + ".manifest.json"

	now := time.Now().UTC().Format(time.RFC3339)

	if t, ok := backupS3Target(); ok {
		if err := backupS3Upload(t, archiveName, uploadPath); err != nil {
			results["s3"] = "خطا: " + err.Error()
		} else {
			_ = backupS3Upload(t, manifestName, manifestPath)
			_ = backupAppendIndexEntry(backupIndexEntry{Cadence: cadence, Destination: "s3", Key: archiveName, ManifestKey: manifestName, Timestamp: now, SizeBytes: size, Encrypted: encrypted, Paths: paths})
			results["s3"] = "آپلود شد"
		}
	}
	if t, ok := backupR2Target(); ok {
		if err := backupS3Upload(t, archiveName, uploadPath); err != nil {
			results["cloudflare_r2"] = "خطا: " + err.Error()
		} else {
			_ = backupS3Upload(t, manifestName, manifestPath)
			_ = backupAppendIndexEntry(backupIndexEntry{Cadence: cadence, Destination: "cloudflare_r2", Key: archiveName, ManifestKey: manifestName, Timestamp: now, SizeBytes: size, Encrypted: encrypted, Paths: paths})
			results["cloudflare_r2"] = "آپلود شد"
		}
	}
	if backupBoolSetting("BACKUP_TELEGRAM_ENABLED", false) {
		botToken := getSetting("BACKUP_TELEGRAM_BOT_TOKEN", "")
		chatID := getSetting("BACKUP_TELEGRAM_CHAT_ID", "")
		if botToken == "" || chatID == "" {
			results["telegram"] = "خطا: bot token یا chat id تنظیم نشده"
		} else {
			fids, mids, err := telegramUploadFile(botToken, chatID, uploadPath, archiveName)
			if err != nil {
				results["telegram"] = "خطا: " + err.Error()
			} else {
				_ = backupAppendIndexEntry(backupIndexEntry{
					Cadence: cadence, Destination: "telegram", Key: strings.Join(fids, ","),
					TelegramMsgIDs: mids, Timestamp: now, SizeBytes: size, Encrypted: encrypted, Paths: paths,
				})
				results["telegram"] = fmt.Sprintf("آپلود شد (%d قطعه)", len(fids))
			}
		}
	}

	if len(results) == 0 {
		return results, fmt.Errorf("هیچ مقصد فعالی برای بکاپ تنظیم نشده")
	}

	if cadence != "manual" {
		for _, cc := range backupCadences() {
			if cc.Name == cadence {
				backupPruneDestinations(cadence, cc.Keep)
			}
		}
	}
	return results, nil
}

func backupPruneDestinations(cadence string, keep int) {
	if keep <= 0 {
		return
	}
	for _, dest := range []string{"s3", "cloudflare_r2", "telegram"} {
		backupIndexMu.Lock()
		idx := backupLoadIndex()
		var matched []int
		for i, e := range idx.Entries {
			if e.Destination == dest && e.Cadence == cadence {
				matched = append(matched, i)
			}
		}
		if len(matched) <= keep {
			backupIndexMu.Unlock()
			continue
		}
		sort.Slice(matched, func(a, b int) bool {
			return idx.Entries[matched[a]].Timestamp > idx.Entries[matched[b]].Timestamp
		})
		toDelete := map[int]bool{}
		for _, i := range matched[keep:] {
			toDelete[i] = true
		}
		remaining := make([]backupIndexEntry, 0, len(idx.Entries))
		var deleted []backupIndexEntry
		for i, e := range idx.Entries {
			if toDelete[i] {
				deleted = append(deleted, e)
				continue
			}
			remaining = append(remaining, e)
		}
		idx.Entries = remaining
		_ = backupSaveIndex(idx)
		backupIndexMu.Unlock()

		for _, e := range deleted {
			backupDeleteRemote(e)
		}
	}
}

func backupDeleteRemote(e backupIndexEntry) {
	switch e.Destination {
	case "s3":
		if t, ok := backupS3Target(); ok {
			_ = backupS3Delete(t, e.Key)
			if e.ManifestKey != "" {
				_ = backupS3Delete(t, e.ManifestKey)
			}
		}
	case "cloudflare_r2":
		if t, ok := backupR2Target(); ok {
			_ = backupS3Delete(t, e.Key)
			if e.ManifestKey != "" {
				_ = backupS3Delete(t, e.ManifestKey)
			}
		}
	case "telegram":
		botToken := getSetting("BACKUP_TELEGRAM_BOT_TOKEN", "")
		chatID := getSetting("BACKUP_TELEGRAM_CHAT_ID", "")
		if botToken == "" {
			return
		}
		for _, mid := range e.TelegramMsgIDs {
			_ = telegramDeleteMessage(botToken, chatID, mid)
		}
	}
}

// restoreBackupEntry آرشیو یک entry مشخص از ایندکس را دانلود، در صورت نیاز
// رمزگشایی، و روی مسیرهای مطلق اصلی extract می‌کند (فایل‌های موجود overwrite می‌شوند).
func restoreBackupEntry(e backupIndexEntry) error {
	tmpDir, err := os.MkdirTemp("", "singbox-restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	downloaded := filepath.Join(tmpDir, "archive.download")
	switch e.Destination {
	case "s3":
		t, ok := backupS3Target()
		if !ok {
			return fmt.Errorf("مقصد s3 دیگر فعال/پیکربندی‌شده نیست")
		}
		if err := backupS3Download(t, e.Key, downloaded); err != nil {
			return err
		}
	case "cloudflare_r2":
		t, ok := backupR2Target()
		if !ok {
			return fmt.Errorf("مقصد cloudflare_r2 دیگر فعال/پیکربندی‌شده نیست")
		}
		if err := backupS3Download(t, e.Key, downloaded); err != nil {
			return err
		}
	case "telegram":
		botToken := getSetting("BACKUP_TELEGRAM_BOT_TOKEN", "")
		if botToken == "" {
			return fmt.Errorf("Telegram bot token دیگر تنظیم نشده")
		}
		fids := strings.Split(e.Key, ",")
		if err := telegramDownloadParts(botToken, fids, downloaded); err != nil {
			return err
		}
	default:
		return fmt.Errorf("مقصد ناشناخته: %s", e.Destination)
	}

	tarPath := downloaded
	if e.Encrypted {
		pass := getSetting("BACKUP_PASSPHRASE", "")
		if pass == "" {
			return fmt.Errorf("این بکاپ رمزگذاری‌شده است اما BACKUP_PASSPHRASE تنظیم نشده")
		}
		decPath := filepath.Join(tmpDir, "archive.tar.gz")
		if err := backupDecryptFile(pass, downloaded, decPath); err != nil {
			return err
		}
		tarPath = decPath
	}
	return backupExtractTarGz(tarPath)
}

// backupLatestEntry جدیدترین entry ایندکس را برمی‌گرداند (بدون فیلتر مقصد/بازه).
func backupLatestEntry() (backupIndexEntry, bool) {
	idx := backupLoadIndex()
	if len(idx.Entries) == 0 {
		return backupIndexEntry{}, false
	}
	best := idx.Entries[0]
	for _, e := range idx.Entries[1:] {
		if e.Timestamp > best.Timestamp {
			best = e
		}
	}
	return best, true
}

// restoreOnInitIfNeeded دقیقاً همان چیزیه که در init باید "بررسی و اعمال" شود:
// اگر همه‌ی مسیرهای پیش‌فرض بکاپ (/data و /app/data) خالی/ناموجودند و حداقل
// یک بکاپ قبلی در ایندکس محلی ثبت شده، آخرین نسخه را خودکار بازیابی می‌کند.
// عمداً محتاطانه است: اگر داده‌ای از قبل روی دیسک باشد، دست به آن نمی‌زند —
// یعنی هیچ‌وقت به‌صورت خاموش چیزی را overwrite نمی‌کند مگر واقعاً یک نصب تازه باشد.
func restoreOnInitIfNeeded() {
	empty := true
	for _, p := range backupDefaultPaths() {
		entries, err := os.ReadDir(p)
		if err == nil && len(entries) > 0 {
			empty = false
			break
		}
	}
	if !empty {
		return
	}
	entry, ok := backupLatestEntry()
	if !ok {
		return // نه بکاپ قبلی‌ای هست نه چیزی برای بازیابی — این یعنی واقعاً نصب تازه است
	}
	log.Printf("💾 restore-on-init: /data و /app/data خالی هستند؛ بازیابی آخرین بکاپ (%s، مقصد %s، %s)…", entry.Cadence, entry.Destination, entry.Timestamp)
	if err := restoreBackupEntry(entry); err != nil {
		log.Printf("⚠️  restore-on-init failed: %v (کانتینر با حالت تازه ادامه می‌دهد)", err)
		return
	}
	log.Println("✅ restore-on-init: بازیابی با موفقیت انجام شد.")
}

// startBackupScheduler هر دقیقه چک می‌کند کدام بازه (ساعتی/روزانه/ماهانه) از
// آخرین اجرایش گذشته و لازم است دوباره بکاپ بگیرد. زمان آخرین اجرای هر بازه
// در همان state.json (EnvOverrides با کلید داخلی BACKUP_LAST_RUN_<cadence>)
// نگه داشته می‌شود تا بعد از ری‌استارت کانتینر هم حفظ شود.
func startBackupScheduler() {
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			if !backupAnyDestinationEnabled() {
				continue
			}
			for _, cc := range backupCadences() {
				if !cc.Enabled {
					continue
				}
				key := "BACKUP_LAST_RUN_" + strings.ToUpper(cc.Name)
				last := getSetting(key, "")
				due := true
				if last != "" {
					if t, err := time.Parse(time.RFC3339, last); err == nil {
						due = time.Since(t) >= cc.Period
					}
				}
				if !due {
					continue
				}
				log.Printf("💾 backup scheduler: running %s backup…", cc.Name)
				results, err := runBackupNow(cc.Name)
				if err != nil {
					log.Printf("⚠️  %s backup failed: %v", cc.Name, err)
					continue
				}
				_ = setSetting(key, time.Now().UTC().Format(time.RFC3339))
				log.Printf("✅ %s backup done: %v", cc.Name, results)
			}
		}
	}()
}

// ---------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------

func backupSettingsHandler(w http.ResponseWriter, r *http.Request) {
	out := map[string]interface{}{
		"paths_default":  backupDefaultPaths(),
		"paths_extra":    backupExtraPaths(),
		"passphrase_set": getSetting("BACKUP_PASSPHRASE", "") != "",
	}
	cadences := map[string]interface{}{}
	for _, cc := range backupCadences() {
		cadences[cc.Name] = map[string]interface{}{"enabled": cc.Enabled, "keep": cc.Keep}
	}
	out["cadences"] = cadences

	_, s3ok := backupS3Target()
	out["s3"] = map[string]interface{}{
		"enabled": backupBoolSetting("BACKUP_S3_ENABLED", false), "configured": s3ok,
		"endpoint": getSetting("BACKUP_S3_ENDPOINT", ""), "region": getSetting("BACKUP_S3_REGION", ""),
		"bucket": getSetting("BACKUP_S3_BUCKET", ""), "prefix": getSetting("BACKUP_S3_PREFIX", ""),
		"use_ssl":        backupBoolSetting("BACKUP_S3_USE_SSL", true),
		"access_key_set": getSetting("BACKUP_S3_ACCESS_KEY", "") != "", "secret_key_set": getSetting("BACKUP_S3_SECRET_KEY", "") != "",
	}
	_, r2ok := backupR2Target()
	out["cloudflare_r2"] = map[string]interface{}{
		"enabled": backupBoolSetting("BACKUP_R2_ENABLED", false), "configured": r2ok,
		"bucket": getSetting("BACKUP_R2_BUCKET", ""), "prefix": getSetting("BACKUP_R2_PREFIX", ""),
		"access_key_set": getSetting("BACKUP_R2_ACCESS_KEY", "") != "", "secret_key_set": getSetting("BACKUP_R2_SECRET_KEY", "") != "",
		"cloudflare_token_available": strings.TrimSpace(readStateOrDefault().Cloudflare.APIToken) != "",
	}
	out["telegram"] = map[string]interface{}{
		"enabled":       backupBoolSetting("BACKUP_TELEGRAM_ENABLED", false),
		"bot_token_set": getSetting("BACKUP_TELEGRAM_BOT_TOKEN", "") != "", "chat_id": getSetting("BACKUP_TELEGRAM_CHAT_ID", ""),
	}

	idx := backupLoadIndex()
	sort.Slice(idx.Entries, func(i, j int) bool { return idx.Entries[i].Timestamp > idx.Entries[j].Timestamp })
	if len(idx.Entries) > 100 {
		idx.Entries = idx.Entries[:100]
	}
	out["backups"] = idx.Entries

	jsonResponse(w, http.StatusOK, out)
}

func updateBackupSettingsHandler(w http.ResponseWriter, r *http.Request) {
	var req map[string]string
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Invalid JSON"})
		return
	}
	allowed := map[string]bool{}
	for _, k := range backupEnvKeys {
		allowed[k] = true
	}
	for key, value := range req {
		if !allowed[key] {
			jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": fmt.Sprintf("%q is not a backup setting", key)})
			return
		}
		if err := setSetting(key, value); err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"error": "Failed to save: " + err.Error()})
			return
		}
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"message": "تنظیمات بکاپ ذخیره شد"})
}

func backupRunHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Cadence string `json:"cadence"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Cadence == "" {
		req.Cadence = "manual"
	}
	results, err := runBackupNow(req.Cadence)
	if err != nil {
		jsonResponse(w, http.StatusBadGateway, map[string]interface{}{"error": err.Error(), "results": results})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"message": "بکاپ انجام شد", "results": results})
}

func backupRestoreHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Destination string `json:"destination"`
		Key         string `json:"key"`
		Timestamp   string `json:"timestamp"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Invalid JSON"})
		return
	}
	idx := backupLoadIndex()
	var found *backupIndexEntry
	for i := range idx.Entries {
		e := idx.Entries[i]
		if e.Destination == req.Destination && e.Key == req.Key && e.Timestamp == req.Timestamp {
			found = &idx.Entries[i]
			break
		}
	}
	if found == nil {
		jsonResponse(w, http.StatusNotFound, map[string]interface{}{"error": "این بکاپ در ایندکس پیدا نشد"})
		return
	}
	if err := restoreBackupEntry(*found); err != nil {
		jsonResponse(w, http.StatusBadGateway, map[string]interface{}{"error": "بازیابی ناموفق: " + err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"message": "بازیابی با موفقیت انجام شد. توصیه می‌شود مدیر را ری‌استارت کنید تا تنظیمات تازه‌بازیابی‌شده بارگذاری شوند."})
}

func backupCreateR2BucketHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Bucket string `json:"bucket"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "Invalid JSON"})
		return
	}
	bucket := strings.ToLower(strings.TrimSpace(req.Bucket))
	if bucket == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "نام bucket لازم است"})
		return
	}
	token, accountID, err := backupResolveCFAccount()
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": err.Error()})
		return
	}
	if _, err := cfAPIRequest(token, http.MethodPost, "/accounts/"+accountID+"/r2/buckets", map[string]interface{}{"name": bucket}); err != nil {
		jsonResponse(w, http.StatusBadGateway, map[string]interface{}{"error": "ساخت bucket ناموفق بود (مطمئن شوید API Token دسترسی «Workers R2 Storage:Edit» دارد): " + err.Error()})
		return
	}
	if err := setSetting("BACKUP_R2_BUCKET", bucket); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"error": "bucket ساخته شد ولی ذخیره‌ی تنظیمات ناموفق بود: " + err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"message": fmt.Sprintf("bucket %q ساخته و به‌عنوان مقصد R2 ست شد. توجه: کلیدهای S3 API (Access Key/Secret Key) این توکن نیستند و باید جدا از R2 → Manage API tokens ساخته و اینجا وارد شوند.", bucket)})
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	// override های ذخیره‌شده از صفحه‌ی Settings را قبل از هر استفاده‌ای از getSetting بارگذاری کن.
	loadEnvOverridesCache()

	// BIND_ADDR/API_PORT/PROXY_PORT فقط از استارت بعدی مدیر اعمال می‌شوند (روی خود
	// socketهای شنود اثر دارند)، برای همین همینجا و قبل از ساخت http.Serverها
	// override احتمالی را روی متغیرهای واقعی اعمال می‌کنیم.
	bindAddr = getSetting("BIND_ADDR", bindAddr)
	apiPort = ":" + strings.TrimPrefix(getSetting("API_PORT", strings.TrimPrefix(apiPort, ":")), ":")
	proxyPort = getSetting("PROXY_PORT", proxyPort)

	// اگر قبلاً از صفحه‌ی Settings توکنی ذخیره شده باشد، بر متغیر محیطی ADMIN_TOKEN اولویت دارد.
	if persisted := readStateOrDefault().AdminToken; persisted != "" {
		adminToken = persisted
	}

	// بررسی و اعمال بازیابی خودکار: اگر /data و /app/data خالی‌اند و بکاپ قبلی
	// در دسترس است، همین‌جا (قبل از ساخت فایل‌های پیش‌فرض/استارت sing-box)
	// بازیابی می‌شود. باید قبل از ensureDefaultFiles اجرا شود وگرنه آن تابع
	// خودش template.json/nodes.json خالی می‌سازد و شرط "خالی بودن" را عملاً
	// از بین می‌برد.
	restoreOnInitIfNeeded()
	// اگر state.json تازه‌بازیابی‌شده زیر /data بوده، override های ذخیره‌شده
	// از UI (شامل خودِ تنظیمات بکاپ) را دوباره بارگذاری کن.
	loadEnvOverridesCache()

	ensureDefaultFiles()
	autoDownloadSingBoxIfMissing()
	startBackupScheduler()

	if getAdminToken() == "" {
		log.Println("⚠️  ADMIN_TOKEN تنظیم نشده — API مدیریت بدون احراز هویت است. چون پنل حالا از طریق reverse proxy عمومی (پورت " + proxyPort + ") هم در دسترس است، این را قبل از فعال‌کردن هر تونلی از صفحه‌ی Settings یا با export ADMIN_TOKEN=... ست کنید.")
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
	// Docker service endpoints removed
	http.HandleFunc("/api/subscriptions", requireAuth(requireMethod(http.MethodGet, listSubscriptionsHandler)))
	http.HandleFunc("/api/add_subscription", requireAuth(requireMethod(http.MethodPost, addSubscriptionHandler)))
	http.HandleFunc("/api/edit_subscription", requireAuth(requireMethod(http.MethodPost, editSubscriptionHandler)))
	http.HandleFunc("/api/refresh_subscription", requireAuth(requireMethod(http.MethodPost, refreshSubscriptionHandler)))
	http.HandleFunc("/api/delete_subscription", requireAuth(requireMethod(http.MethodPost, deleteSubscriptionHandler)))
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
	http.HandleFunc("/api/warp/endpoint", requireAuth(requireMethod(http.MethodGet, getWarpEndpointHandler)))
	http.HandleFunc("/api/warp/endpoint/ping_current", requireAuth(requireMethod(http.MethodPost, pingCurrentWarpEndpointHandler)))
	http.HandleFunc("/api/warp/endpoint/backups", requireAuth(requireMethod(http.MethodGet, getWarpBackupsHandler)))
	http.HandleFunc("/api/warp/endpoint/test_backups", requireAuth(requireMethod(http.MethodPost, testWarpBackupsHandler)))
	http.HandleFunc("/api/warp/endpoint/scan_batch", requireAuth(requireMethod(http.MethodPost, scanWarpBatchHandler)))
	http.HandleFunc("/api/warp/endpoint/apply", requireAuth(requireMethod(http.MethodPost, applyWarpEndpointHandler)))
	http.HandleFunc("/api/warp/endpoint/reset", requireAuth(requireMethod(http.MethodPost, resetWarpEndpointHandler)))
	http.HandleFunc("/api/backup/settings", requireAuth(settingsMethodRouter(backupSettingsHandler, updateBackupSettingsHandler)))
	http.HandleFunc("/api/backup/run", requireAuth(requireMethod(http.MethodPost, backupRunHandler)))
	http.HandleFunc("/api/backup/restore", requireAuth(requireMethod(http.MethodPost, backupRestoreHandler)))
	http.HandleFunc("/api/backup/r2/create_bucket", requireAuth(requireMethod(http.MethodPost, backupCreateR2BucketHandler)))

	server := &http.Server{Addr: bindAddr + apiPort}

	go func() {
		log.Printf("🚀 Manager running on http://%s%s", bindAddr, apiPort)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("Server failed: %v", err)
			singBoxCmdMu.Lock()
			stopProcess(runningSingBox)
			runningSingBox = nil
			singBoxCmdMu.Unlock()
			os.Exit(1)
		}
	}()

	startReverseProxyServer()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	<-sigCh

	log.Println("Shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
	if reverseProxyServer != nil {
		_ = reverseProxyServer.Shutdown(ctx)
	}

	singBoxCmdMu.Lock()
	stopProcess(runningSingBox)
	runningSingBox = nil
	singBoxCmdMu.Unlock()

	log.Println("Goodbye.")
}

type WarpScanItem struct {
	Endpoint  string `json:"endpoint"`
	Delay     int    `json:"delay"`
	IsDefault bool   `json:"is_default"`
	IsBackup  bool   `json:"is_backup"`
}

func isIPv6Endpoint(ep string) bool {
	return strings.Contains(ep, "[") || strings.Count(ep, ":") > 1
}

func generateBatchWarpCandidates(count int, excluded map[string]bool, ipType string) ([]string, map[string]bool) {
	state := readStateOrDefault()

	backupsMap := make(map[string]bool)
	for _, b := range state.BackupWarpEndpoints {
		backupsMap[b] = true
	}

	var candidates []string
	seen := make(map[string]bool)

	addCandidate := func(ep string) {
		if ep == "" || excluded[ep] || seen[ep] {
			return
		}
		isV6 := isIPv6Endpoint(ep)
		if ipType == "ipv4" && isV6 {
			return
		}
		if ipType == "ipv6" && !isV6 {
			return
		}
		seen[ep] = true
		candidates = append(candidates, ep)
	}

	// 1. Default reference
	defaultEP := "engage.cloudflareclient.com:2408"
	addCandidate(defaultEP)

	// 2. Saved Backup Endpoints (Highest priority)
	for _, b := range state.BackupWarpEndpoints {
		addCandidate(b)
	}

	// 3. Known working Cloudflare WARP IPs
	defaults := []string{
		"162.159.192.1:2408",
		"162.159.193.3:2408",
		"162.159.195.1:2408",
		"162.159.192.1:1701",
		"162.159.193.3:1701",
		"162.159.195.1:1701",
		"162.159.192.1:500",
		"162.159.193.3:500",
		"162.159.195.1:500",
		"188.114.96.1:2408",
		"188.114.97.1:2408",
	}
	for _, ep := range defaults {
		addCandidate(ep)
	}

	// 4. Fill up to `count` with random Cloudflare IPs
	ports := []int{
		500, 854, 859, 864, 878, 880, 890, 891, 894, 903,
		908, 928, 934, 939, 942, 943, 945, 946, 955, 968,
		987, 988, 1002, 1010, 1014, 1018, 1070, 1074, 1180, 1387,
		1701, 1843, 2371, 2408, 2506, 3138, 3476, 3581, 3854, 4177,
		4198, 4233, 4500, 5279, 5956, 7103, 7152, 7156, 7281, 7559, 8319, 8742, 8854, 8886,
	}

	ipv4Prefixes := []string{
		"188.114.96.", "188.114.97.", "188.114.98.", "188.114.99.",
		"162.159.192.", "162.159.193.", "162.159.195.", "8.34.146.",
		"8.39.214.", "8.39.204.", "8.6.112.", "8.35.211.", "8.39.125.",
		"8.47.69.",
	}

	ipv6Prefixes := []string{
		"2606:4700:d0::", "2606:4700:d1::",
	}

	r := mathrand.New(mathrand.NewSource(time.Now().UnixNano()))

	maxAttempts := count * 50
	if maxAttempts < 1000 {
		maxAttempts = 1000
	}
	attempts := 0
	for len(candidates) < count && attempts < maxAttempts {
		attempts++
		var ep string
		makeV6 := false
		if ipType == "ipv6" {
			makeV6 = true
		} else if ipType == "both" {
			makeV6 = (r.Intn(2) == 1)
		}

		if makeV6 {
			prefix := ipv6Prefixes[r.Intn(len(ipv6Prefixes))]
			ip := fmt.Sprintf("[%s%x:%x:%x:%x]", prefix,
				r.Intn(65536), r.Intn(65536),
				r.Intn(65536), r.Intn(65536))
			ep = fmt.Sprintf("%s:%d", ip, ports[r.Intn(len(ports))])
		} else {
			prefix := ipv4Prefixes[r.Intn(len(ipv4Prefixes))]
			ip := fmt.Sprintf("%s%d", prefix, r.Intn(256))
			ep = fmt.Sprintf("%s:%d", ip, ports[r.Intn(len(ports))])
		}
		addCandidate(ep)
	}

	return candidates, backupsMap
}

func scanBatchWarpEndpoints(batchSize int, excludedEndpoints []string, ipType string) ([]WarpScanItem, int, error) {
	mu.Lock()
	var nodes []map[string]interface{}
	_ = readJSON(nodesFile, &nodes)
	mu.Unlock()

	var sampleWarp map[string]interface{}
	for _, n := range nodes {
		if t, ok := n["type"].(string); ok && t == "wireguard" {
			sampleWarp = n
			break
		}
	}

	if sampleWarp == nil {
		return nil, 0, fmt.Errorf("no existing WARP node found to extract account details for testing")
	}

	excludedMap := make(map[string]bool)
	for _, ep := range excludedEndpoints {
		excludedMap[ep] = true
	}

	endpoints, backupsMap := generateBatchWarpCandidates(batchSize, excludedMap, ipType)
	if len(endpoints) == 0 {
		return []WarpScanItem{}, 0, nil
	}

	var endpointNodes []map[string]interface{}
	var outboundTags []string

	var peerTemplate map[string]interface{}
	if peers, ok := sampleWarp["peers"].([]interface{}); ok && len(peers) > 0 {
		if p, ok := peers[0].(map[string]interface{}); ok {
			peerTemplate = p
		}
	}

	for i, ep := range endpoints {
		tag := fmt.Sprintf("WARP_TEST_%d", i)
		outboundTags = append(outboundTags, tag)

		host, port, _ := parseEndpoint(ep)

		peer := map[string]interface{}{
			"address": host,
			"port":    port,
		}
		if peerTemplate != nil {
			if val, ok := peerTemplate["public_key"]; ok { peer["public_key"] = val }
			if val, ok := peerTemplate["allowed_ips"]; ok { peer["allowed_ips"] = val }
			if val, ok := peerTemplate["reserved"]; ok { peer["reserved"] = val }
		}

		ob := map[string]interface{}{
			"type":  "wireguard",
			"tag":   tag,
			"peers": []interface{}{peer},
		}

		if val, ok := sampleWarp["address"]; ok { ob["address"] = val }
		if val, ok := sampleWarp["private_key"]; ok { ob["private_key"] = val }
		if val, ok := sampleWarp["mtu"]; ok { ob["mtu"] = val }

		endpointNodes = append(endpointNodes, ob)
	}

	// Add urltest outbound
	outbounds := []map[string]interface{}{
		{
			"type":      "urltest",
			"tag":       "test_urltest",
			"outbounds": outboundTags,
			"url":       "http://cp.cloudflare.com/generate_204",
			"interval":  "3m",
		},
	}
	apiPort := 9095
	cfg := map[string]interface{}{
		"endpoints": endpointNodes,
		"outbounds": outbounds,
		"experimental": map[string]interface{}{
			"clash_api": map[string]interface{}{
				"external_controller": fmt.Sprintf("127.0.0.1:%d", apiPort),
			},
		},
	}

	tmpFile := filepath.Join(filepath.Dir(nodesFile), "temp_warp_scan.json")
	b, _ := json.MarshalIndent(cfg, "", "  ")
	_ = os.WriteFile(tmpFile, b, 0644)
	defer os.Remove(tmpFile)

	singboxBin, _ := findSingBox()
	if singboxBin == "" {
		return nil, 0, fmt.Errorf("sing-box binary not found")
	}
	cmd := exec.Command(singboxBin, "run", "-c", tmpFile)
	if err := cmd.Start(); err != nil {
		return nil, 0, fmt.Errorf("failed to start temporary sing-box for scanning: %w", err)
	}

	defer func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
	}()

	time.Sleep(2 * time.Second)

	// Trigger delay test
	testReqURL := fmt.Sprintf("http://127.0.0.1:%d/proxies/test_urltest/delay?timeout=3000&url=http://cp.cloudflare.com/generate_204", apiPort)
	if testResp, err := http.Get(testReqURL); err == nil {
		testResp.Body.Close()
	}

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/proxies", apiPort))
	if err != nil {
		return nil, 0, fmt.Errorf("failed to reach temporary clash api: %w", err)
	}
	defer resp.Body.Close()

	var apiResp struct {
		Proxies map[string]struct {
			Now     string `json:"now"`
			All     []string `json:"all"`
			History []struct {
				Time  string `json:"time"`
				Delay int    `json:"delay"`
			} `json:"history"`
		} `json:"proxies"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, 0, fmt.Errorf("failed to decode proxies api response: %w", err)
	}

	var validResults []WarpScanItem
	state := readStateOrDefault()
	backupChanged := false
	existingBackups := make(map[string]bool)
	for _, b := range state.BackupWarpEndpoints {
		existingBackups[b] = true
	}

	for i, ep := range endpoints {
		tag := fmt.Sprintf("WARP_TEST_%d", i)
		if p, ok := apiResp.Proxies[tag]; ok && len(p.History) > 0 {
			lastH := p.History[len(p.History)-1]
			if lastH.Delay > 0 {
				validResults = append(validResults, WarpScanItem{
					Endpoint:  ep,
					Delay:     lastH.Delay,
					IsDefault: ep == "engage.cloudflareclient.com:2408",
					IsBackup:  backupsMap[ep],
				})

				// Save as backup if not already present
				if !existingBackups[ep] && ep != "engage.cloudflareclient.com:2408" {
					state.BackupWarpEndpoints = append(state.BackupWarpEndpoints, ep)
					existingBackups[ep] = true
					backupChanged = true
				}
			}
		}
	}

	if backupChanged {
		_ = writeState(state)
	}

	// Sort valid results by delay ascending
	sort.Slice(validResults, func(i, j int) bool {
		return validResults[i].Delay < validResults[j].Delay
	})

	return validResults, len(endpoints), nil
}

func scanWarpBatchHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BatchSize int      `json:"batch_size"`
		Excluded  []string `json:"excluded"`
		IPType    string   `json:"ip_type"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.BatchSize <= 0 {
		req.BatchSize = 25
	}
	if req.IPType == "" {
		req.IPType = "both"
	}

	results, testedCount, err := scanBatchWarpEndpoints(req.BatchSize, req.Excluded, req.IPType)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"error": err.Error()})
		return
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"valid_results": results,
		"tested_count":  testedCount,
	})
}
