# دیپلوی vpn-ui روی Railway

[![Deploy on Railway](https://railway.com/button.svg)](https://railway.com/new/template?template=https://github.com/EMIXPI/vpn-ui&utm_medium=integration&utm_source=button&utm_campaign=generic)

> 📖 English quick-reference is at the bottom of this file.

این شاخه، پروفایل مخصوص Railway پروژه را اضافه می‌کند: فقط بخش‌هایی که داخل کانتینر واقعاً کار می‌کنند نگه داشته شده‌اند و بقیهٔ دیپلوی — بیلد، متغیرها، پورت، دیتابیس — با **یک دکمه** انجام می‌شود.

## چه چیزی روی Railway کار می‌کند؟

Railway کانتینر بدون دسترسی به کرنل اجرا می‌کند (بدون TUN، بدون ماژول کرنل، بدون NET_ADMIN). بنابراین:

| بخش | وضعیت روی Railway |
|---|---|
| پنل مدیریت کامل (UI، API، چند-ادمین، نماینده/Reseller، ربات تلگرام، بکاپ) | ✅ |
| VLESS / VMess / Trojan / Shadowsocks (با AES-GCM) / Socks / Http / XHTTP | ✅ |
| AnyTLS / TUIC v5 / NaiveProxy (پروتکل‌های native هستهٔ patched) | ✅ (TUIC نیازمند UDP عمومی است — پایین را ببینید) |
| سرور اشتراک (Subscription / JSON / Clash) | ✅ |
| هستهٔ Xray پچ‌شده (کامیت پین‌شدهٔ `Sir-MmD/Xray-core`) + فایل‌های geo | ✅ داخل خود باینری بیک می‌شود |
| PPTP / L2TP / L2TP-IPsec / OpenVPN / OpenConnect / SSTP / IKEv2 / WireGuard (کرنل) / AmneziaWG / GRE | ❌ به کرنل و TUN نیاز دارند؛ پنل این هسته‌ها را «نصب‌نشده» نشان می‌دهد |
| MTProto (telemt) و SSH | ❌ در بیلد Railway لحاظ نشده‌اند |

## شروع سریع (یک کلیک)

1. روی دکمهٔ **Deploy on Railway** بالا بزنید (یا از [Templates](https://railway.com/templates) یک قالب از روی همین ریپو بسازید).
2. Railway سرویس را از `deploy/railway/Dockerfile` می‌سازد؛ همه‌چیز داخل بیلد اتفاق می‌افتد: کامپایل هستهٔ Xray از کامیت پین‌شده، دانلود فایل‌های geo، بیک‌کردن همه در باینری، و ست‌کردن پورت از متغیر `PORT` خود Railway.
3. بعد از بالا آمدن، دامنهٔ Railway را باز کنید و با `admin` / `admin` وارد شوید (اگر `VPNUI_ADMIN_PASSWORD` را ست نکرده‌اید، **بلافاصله پسورد را عوض کنید**).

### نکتهٔ مهم: Volume ببندید

بدون Volume، دیتابیس روی فایل‌سیستمِ موقت خود کانتینر (پوشهٔ `/data` داخل کانتینر) ذخیره می‌شود و با هر Redeploy پاک می‌شود:

- در Railway: سرویس → **Volumes** → مسیر `/data` را وصل کنید.

دیتابیس در `/data/vpn-ui.db`، لاگ‌ها در `/data/logs` و باینری هستهٔ Xray در `/data/bin` ذخیره می‌شود. اگر `/data` به هر دلیلی قابل‌نوشتن نباشد، اسکریپت اجرا به‌جای آن از `/app/data` استفاده می‌کند و در لاگ هشدار صریح می‌دهد.

## متغیرهای محیطی

### متغیرهای عمومی (در Railway → Variables)

| متغیر | پیش‌فرض | توضیح |
|---|---|---|
| `PORT` | خودکار از Railway | پورت پنل؛ در هر اجرا با پورت واقعی سرویسِ Railway همگام می‌شود. دست نزنید. |
| `VPNUI_ADMIN_USERNAME` | `admin` | فقط دفعهٔ اول؛ اگر با `VPNUI_ADMIN_PASSWORD` همراه باشد، ادمین با همین نام ساخته می‌شود. |
| `VPNUI_ADMIN_PASSWORD` | — | فقط دفعهٔ اول؛ **هر دو** متغیر باید ست شوند تا ادمین ساخته شود. در قالب Railway برایش Random بگذارید. |
| `VPNUI_WEB_BASE_PATH` | `/` | فقط دفعهٔ اول؛ مثلا `/panel/` برای مخفی‌کردن پنل پشت یک مسیر. |
| `VPNUI_DATA_DIR` | `/data` | ریشهٔ داده‌ها؛ فقط اگر مسیر Volume دیگری دارید عوض کنید. |
| `VPNUI_DB_FOLDER` / `VPNUI_LOG_FOLDER` / `VPNUI_BIN_FOLDER` | زیرمجموعهٔ `VPNUI_DATA_DIR` | اگر بخواهید هر کدام را جدا کنید. |
| `VPNUI_LOG_LEVEL` | `info` | سطح لاگ (`debug` / `info` / ...). |

«فقط دفعهٔ اول» یعنی با فایل نشانگر `/data/.railway-provisioned` فقط یک‌بار اعمال می‌شود؛ پس از آن تغییر پسورد از داخل پنل در ری‌استارت‌ها حفظ می‌شود.

### آرگومان‌های بیلد (Build Args — اختیاری)

| آرگومان | پیش‌فرض | توضیح |
|---|---|---|
| `GEO_IR` | `0` | `1` = فایل‌های `geoip_IR.dat` و `geosite_IR.dat` داخل باینری بیک شوند. |
| `GEO_RU` | `0` | `1` = فایل‌های روسیه داخل باینری بیک شوند. |
| `GO_VERSION` | `1.26` | نسخهٔ Go برای بیلد. |
| `XRAY_REPO` / `XRAY_COMMIT` | ریپو/کامیت پین‌شده | هستهٔ Xray پچ‌شده؛ دست نزنید مگر اینکه پین را عوض کرده باشید. |

پیش‌فرض فقط جفت پایهٔ `geoip.dat`/`geosite.dat` را بیک می‌کند (لایتنر). پنل هر فایل کشوری را **خودش** دفعهٔ اول که یک قانون روتینگ به آن اشاره کند دانلود می‌کند؛ چون Runtime شما روی Railway است (نه داخل ایران)، این دانلود مشکلی ندارد.

## شبکه: پورت پنل و پورت‌های Inbound

- **پنل:** دامنهٔ پیش‌فرض Railway به پورت `PORT` وصل است. پنل روی `0.0.0.0:$PORT` گوش می‌دهد — چیزی برای تنظیم نیست.
- **ترافیک Inbound ها:** برای هر اینباند باید پورت را هم در Railway باز کنید:
  1. در پنل، اینباند را روی یک پورت بسازید (مثلاً `8443`).
  2. در Railway: سرویس → **Settings → Networking** → پورت `8443` را اضافه کنید.
  3. Railway یک دامنه/پورت عمومی (مثلاً `xxxx.proxy.rlwy.net:port`) می‌دهد؛ کلاینت‌ها با همان `host:port` وصل می‌شوند.
- **پروکسی عمومی Railway برای پروتکل‌های TCP است.** پس پروتکل‌های TCP کامل کار می‌کنند (VLESS/VMess/Trojan/SS/AnyTLS/NaiveProxy-over-h2/XHTTP). TUIC (روی QUIC/UDP) از بیرون قابل‌دسترس نیست؛ برای HTTP/3 هم همین‌طور.
- **اشتراک (Subscription):** از پنل فعالش کنید (پورت پیش‌فرض `2097`)، همان پورت را در Railway باز کنید، و در تنظیمات پنل `subDomain` را روی دامنهٔ Railway بگذارید تا لینک‌های اشتراک درست ساخته شوند. همچنین می‌توانید به‌جای پورت جدا، اشتراک را از طریق دامنهٔ خود پنل هم سرو کنید (تنظیم `subPort` روی همان `PORT` پنل + `subEnable`).

## ورود اولیه و امنیت

- بدون متغیرها: `admin` / `admin` — بلافاصله عوضش کنید.
- بهتر است در همان دیپلویِ یک‌کلیکی، `VPNUI_ADMIN_PASSWORD` را (با جنریتور Random خود Railway) ست کنید و `VPNUI_WEB_BASE_PATH` را مثلاً `/panel/` بگذارید؛ سپس در `railway.json` مقدار `deploy.healthcheckPath` را هم روی همان مسیر تنظیم کنید (چون `/` دیگر پاسخ نمی‌دهد).
- پنل با HTTP پشت پروکسی Railway سرو می‌شود؛ TLS را پروکسی Railway انجام می‌دهد. برای دامنهٔ شخصی، همان دامنه را در Railway وصل کنید.

## بیلد به‌صورت دستی (بدون دکمه)

```bash
railway init
railway up          # railway.json بیلد از Dockerfile و اجرا را خودش می‌چیند
railway volume add /data   # یا از داشبورد
```

## ساخت قالب «یک‌کلیکِ واقعی» (برای مالک ریپو)

دکمهٔ بالا یک قالب از روی ریپو می‌سازد؛ برای اینکه Volume و متغیرها هم از قبل چیده باشند:

1. در Railway به **Templates → New Template** بروید و سرویس را از همین ریپوی گیت‌هاب اضافه کنید.
2. در همان ادیتور قالب: Volume با mount path `/data`، متغیرهای `VPNUI_ADMIN_PASSWORD` (Random) و بقیه را تنظیم کنید، و به پورت پنل دامنه وصل کنید.
3. قالب را Publish کنید؛ کد قالب (مثلاً `AbCdEf`) را بردارید و لینک دکمه را در `README.md` به این شکل عوض کنید:
   `https://railway.com/new/template/AbCdEf?utm_medium=integration&utm_source=button`
4. همین لینک را در `RAILWAY.md` هم به‌روز کنید.

---

## English quick-reference

**What works on Railway:** the full panel (UI/API/multi-admin/reseller/Telegram bot/backups) and every Xray-core protocol — VLESS, VMess, Trojan, Shadowsocks (incl. AES-GCM), SOCKS/HTTP, XHTTP, plus the patched core's native AnyTLS, TUIC v5 and NaiveProxy — served by the pinned patched core baked into the binary. Subscription server included.

**What does not (needs kernel/TUN/NET_ADMIN, so it is excluded from this build):** PPTP, L2TP/IPsec, OpenVPN, OpenConnect, SSTP, IKEv2, kernel WireGuard, AmneziaWG, GRE, MTProto (telemt) and SSH.

**One click:** hit the Deploy button — the `deploy/railway/Dockerfile` build compiles the pinned Xray core, bakes geo files in, and the entrypoint (`deploy/railway/entrypoint.sh`) syncs the panel port to Railway's `PORT`, provisions the admin from env on first boot, and execs the server. Config-as-code lives in `railway.json` (Dockerfile builder, healthcheck on `/`, restart on failure).

**Variables:** `PORT` (automatic), `VPNUI_ADMIN_USERNAME`, `VPNUI_ADMIN_PASSWORD` (first boot, both required), `VPNUI_WEB_BASE_PATH` (first boot), `VPNUI_DATA_DIR` (default `/data`). Build args: `GEO_IR`, `GEO_RU`, `GO_VERSION`, `XRAY_REPO`, `XRAY_COMMIT`.

**Storage:** attach a volume at `/data` — otherwise the DB lives on the container's ephemeral filesystem and is lost on every redeploy (if `/data` is ever unwritable, the entrypoint warns loudly and falls back to `/app/data`).

**Networking:** panel on the generated domain; for each inbound, add a matching TCP port under Service → Settings → Networking (Railway's public proxy is TCP, so UDP-based TUIC/HTTP3 are not reachable from outside). Enable the subscription server in panel settings and expose its port the same way.

**First login:** `admin` / `admin` unless `VPNUI_ADMIN_USERNAME` + `VPNUI_ADMIN_PASSWORD` were set — change the default immediately either way.
