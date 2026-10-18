# Kết nối GitLab (GitLab.com hoặc self-managed)

## 1. Token cho bot

Khuyến nghị dùng **group access token** (Group → Settings → Access tokens), hoặc user bot riêng.

| Thiết lập | Giá trị |
|---|---|
| Role | **Developer** (quyền tối thiểu để set commit status và sửa label) |
| Scope | `api` |

```bash
GITLAB_URL=https://gitlab.com            # hoặc https://gitlab.company.vn
GITLAB_TOKEN=glpat-...
GITLAB_WEBHOOK_SECRET=$(openssl rand -hex 32)
```

## 2. Webhook

Đặt ở **group** (Group → Settings → Webhooks, có trên GitLab Premium) để mọi project con được kiểm tra,
hoặc ở từng project (Project → Settings → Webhooks).

| Trường | Giá trị |
|---|---|
| URL | `https://<host-của-bạn>/webhooks/gitlab` |
| Secret token | giá trị `GITLAB_WEBHOOK_SECRET` |
| Trigger | **Merge request events**, **Issues events**, **Comments** |
| SSL verification | bật |

Bấm *Test → Merge request events* để kiểm tra: service trả `202` (hoặc `204` nếu event bị bỏ qua).

## 3. Thêm phần CI

Trong `.gitlab-ci.yml` của project:

```yaml
include:
  - project: platform/repoguard        # nơi bạn mirror repo repoguard
    ref: v1
    file: ci-templates/gitlab/repoguard.gitlab-ci.yml
```

Job dùng image `ghcr.io/<owner>/repoguard-ci` (Go + Node + Python + osv-scanner + repoguard).
Nếu runner không ra được internet, build image từ `deploy/ci-runner.Dockerfile` và đẩy vào registry nội bộ,
rồi đặt biến `REPOGUARD_IMAGE`.

## 4. Bắt buộc check

Project → Settings → Merge requests → **Pipelines must succeed** để chặn merge khi job `repoguard` (CI) fail.
Status `repoguard/pr-rules`, `repoguard/secrets`, `repoguard/ai-review` của service được GitLab hiển thị
như các job ngoài (stage `external`) trên commit của MR. Hãy thử trên một MR để xác nhận GitLab phiên bản
của bạn tính các status ngoài này vào điều kiện merge như mong muốn.
