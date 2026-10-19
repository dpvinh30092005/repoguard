# Postman, SonarQube, Jenkins

repoguard không gộp code của các công cụ này vào mình. Nó **điều phối** chúng qua CLI và API
chính thức, rồi gom kết quả vào một chỗ: comment trên MR, status, và báo cáo `repoguard ci`.

| Công cụ | repoguard làm gì | Cấu hình |
|---|---|---|
| **Postman** | Chạy mọi `*.postman_collection.json` bằng **Newman** (CLI chính thức của Postman). Có thể tự khởi động API và đợi nó sẵn sàng trước khi test | `.repoguard.yml` → `ci.postman` |
| **SonarQube** | Chạy `sonar-scanner`, đợi Quality Gate, đọc số bug / lỗ hổng / độ phủ qua Web API | Biến môi trường `SONAR_HOST_URL`, `SONAR_TOKEN` + `ci.sonar` |
| **Jenkins** | Cách chạy CI thứ hai (bên cạnh Docker sandbox): kích hoạt job `repoguard-ci`, đợi xong, đọc báo cáo về MR | `REPOGUARD_EXECUTOR=jenkins` + `JENKINS_URL/USER/TOKEN` |

## Postman (Newman)

Đặt collection export từ Postman vào repo (File → Export → Collection v2.1), ví dụ
`api/postman/orders.postman_collection.json`. Nếu test gọi tới API đang chạy, khai báo:

```yaml
ci:
  postman:
    start: npm --prefix api start              # chạy nền, tự tắt sau khi test xong
    wait_url: http://127.0.0.1:3000/health     # đợi tới khi trả lời
    environment: api/postman/local.postman_environment.json
```

Kết quả: bảng *API tests (Postman)* (số request, assertion, số lỗi) và từng assertion fail.
Ví dụ chạy thật: repo [monorepo-ci](https://github.com/dpvinh30092005/monorepo-ci) (`projects/api-node/postman`).

## SonarQube

1. Có SonarQube (tự host, hoặc dùng [bộ dựng sẵn](#bộ-dựng-sẵn-sonarqube--jenkins--repoguard)).
2. Tạo token loại *Global Analysis Token* (My Account → Security), chỉ có quyền phân tích.
3. Đặt `SONAR_HOST_URL` và `SONAR_TOKEN` ở nơi chạy build:
   - executor Docker: biến môi trường của service repoguard (được truyền vào sandbox theo tên, giá trị không xuất hiện trên dòng lệnh)
   - Jenkins: biến môi trường của agent (bộ dựng sẵn đã cấu hình)
   - GitHub Actions / GitLab CI: secret/variable của repo

`fail_on_gate: true` (mặc định) làm build fail khi Quality Gate đỏ.
Lưu ý: bản Community chỉ phân tích nhánh chính; phân tích theo từng MR cần bản Developer trở lên.

## Jenkins

Dùng khi công ty đã có Jenkins (hoặc muốn tách việc build ra khỏi máy chạy repoguard):

```bash
REPOGUARD_EXECUTOR=jenkins
JENKINS_URL=https://jenkins.congty.vn
JENKINS_USER=repoguard-bot
JENKINS_TOKEN=<API token của user đó>
JENKINS_JOB=repoguard-ci            # mặc định
REPOGUARD_EXECUTOR_REPOS=nhom/*     # chỉ các repo này được build
```

Trên Jenkins: tạo Pipeline job theo [`ci-templates/jenkins/Jenkinsfile`](../ci-templates/jenkins/Jenkinsfile)
với các tham số ghi ở đầu file, và credential `repoguard-git` đọc được repo. Agent cần git,
repoguard và toolchain; image `repoguard-ci` có sẵn tất cả.

Luồng: repoguard kiểm tra quyền tác giả MR → gửi `buildWithParameters` (URL repo không kèm
token, ref, commit, policy từ default branch) → đợi job xong → đọc artifact `repoguard-report.json`
→ comment trên MR kèm link tới build Jenkins. Quá thời gian thì repoguard dừng build trên Jenkins.

## Bộ dựng sẵn: SonarQube + Jenkins + repoguard

```bash
cp deploy/stack/.env.example deploy/stack/.env        # điền GitLab token, mật khẩu Jenkins
docker compose -f deploy/stack/docker-compose.yml up -d --build
deploy/stack/init-sonar.sh                             # in ra SONAR_TOKEN → dán vào .env
docker compose -f deploy/stack/docker-compose.yml up -d
```

| Dịch vụ | Địa chỉ | Đăng nhập |
|---|---|---|
| repoguard | http://localhost:47474 | |
| Jenkins | http://localhost:8081 | `admin` / `JENKINS_ADMIN_PASSWORD` |
| SonarQube | http://localhost:9000 | `admin` / mật khẩu do `init-sonar.sh` in ra |

Jenkins được cấu hình sẵn bằng configuration-as-code: user admin, credential `repoguard-git`
(dùng `GITLAB_TOKEN`), job `repoguard-ci`, biến `SONAR_*`. Đây là cấu hình demo (build chạy trên
controller, SonarQube dùng DB nhúng); khi chạy thật hãy dùng agent và database riêng.

Bộ này được kiểm tra thật trên GitHub Actions ở job `stack (real sonarqube + jenkins)`.
