# Kết nối GitHub

## 1. Tạo GitHub App (một lần cho cả org)

GitHub → **Settings → Developer settings → GitHub Apps → New GitHub App**
(hoặc ở cấp org: *Organization settings → GitHub Apps*).

| Trường | Giá trị |
|---|---|
| Webhook URL | `https://<host-của-bạn>/webhooks/github` |
| Webhook secret | một chuỗi ngẫu nhiên (`openssl rand -hex 32`) → `GITHUB_WEBHOOK_SECRET` |

**Repository permissions** (tối thiểu):

| Quyền | Mức | Dùng để |
|---|---|---|
| Contents | Read | đọc `.repoguard.yml` |
| Metadata | Read | bắt buộc |
| Pull requests | Read & write | đọc diff, đăng review |
| Issues | Read & write | comment, label, đóng issue |
| Commit statuses | Read & write | `repoguard/*` status |

**Subscribe to events:** `Pull request`, `Issues`, `Issue comment`.

Sau khi tạo: ghi lại **App ID**, tạo **private key** (.pem), cấu hình service:

```bash
GITHUB_APP_ID=123456
GITHUB_APP_PRIVATE_KEY_FILE=/run/secrets/github-app.pem   # hoặc GITHUB_APP_PRIVATE_KEY="-----BEGIN..."
GITHUB_WEBHOOK_SECRET=...
```

## 2. Cài App vào repo

App settings → **Install App** → chọn org/account → *All repositories* hoặc chọn từng repo.
Repo mới được cài sẽ được kiểm tra ngay từ event tiếp theo, và được sweep stale/SLA ở lượt sweep sau.

## 3. Thêm phần CI (lint/build/test/CVE)

Copy `ci-templates/github/repoguard.yml` vào `.github/workflows/` của repo. Workflow này gọi
reusable workflow `repoguard-ci.yml`, chạy trên runner của repo đó.

## 4. Bắt buộc check trước khi merge

*Settings → Branches → Branch protection rule* (hoặc Rulesets) cho `main`:
require status checks `repoguard/pr-rules`, `repoguard/secrets` và job CI `repoguard / repoguard ci` (tên = `<job id trong workflow của bạn> / repoguard ci`).
Không nên require `repoguard/ai-review` trừ khi đã bật `fail_on_critical`.

## GitHub Enterprise Server

Đặt `GITHUB_API_URL=https://ghe.example.com/api/v3`. Mọi thứ khác giữ nguyên.

## Không dùng App được?

Dùng tài khoản bot: `GITHUB_TOKEN=<fine-grained PAT>` với các quyền như bảng trên, và tạo webhook
ở từng repo/org (*Settings → Webhooks*, content type `application/json`, cùng secret, chọn các event trên).
