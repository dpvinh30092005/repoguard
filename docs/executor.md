# Executor: repoguard tự chạy CI khi GitLab tắt pipeline

Dùng khi project **không chạy được pipeline** (CI/CD bị tắt, không có runner). repoguard sẽ tự
clone đúng commit của MR, chạy `repoguard ci` (lint · build · test · quét secret · CVE) trong một
container dùng một lần, rồi báo kết quả lên MR.

```
MR mới / push mới ──► repoguard service ──► hàng đợi executor (riêng)
                                              │ 1. Fork? Tác giả có quyền Developer?
                                              │ 2. git fetch đúng commit (token không vào container)
                                              │ 3. Thay .repoguard.yml bằng bản trên default branch
                                              │ 4. docker create (giới hạn) → cp source → start
                                              │ 5. Đọc repoguard-report.json → xoá container
                                              ▼
                        Status repoguard/ci + comment "🧪 repoguard ci" trên MR
```

## Hàng rào an toàn

Chạy code của MR trên máy dùng chung là việc rủi ro, nên executor **mặc định tắt** và có các lớp chặn:

| Lớp | Cách làm |
|---|---|
| Danh sách repo được phép | `REPOGUARD_EXECUTOR_REPOS` **bắt buộc**; bật executor mà thiếu biến này thì service không khởi động |
| Ai được build | Chỉ MR có tác giả là Developer trở lên (người vốn đã push được code); MR từ **fork** không bao giờ được build |
| Policy | `.repoguard.yml` lấy từ default branch, MR không thể tự tắt check |
| Credential | Token chỉ dùng để `git fetch` trên host, truyền qua biến môi trường của git. Không nằm trong `.git/config`, argv hay container |
| Container | Mỗi lần chạy một container mới, xoá ngay sau đó; `--cap-drop ALL`, `no-new-privileges`, giới hạn CPU, RAM (không swap), số tiến trình, thời gian; không mount thư mục host |
| Cô lập mạnh hơn (khuyến nghị) | `REPOGUARD_EXECUTOR_RUNTIME=runsc` để chạy bằng gVisor |
| Tài nguyên | Hàng đợi riêng, giới hạn số build song song, nên build nặng không làm chậm phần comment/review |

**Rủi ro còn lại, cần biết:**
- Service phải truy cập Docker socket, tức là có quyền root trên máy đó. **Hãy chạy executor trên một VM riêng.**
- Mặc định container có mạng (`bridge`) để tải dependency. Code trong MR có thể gọi ra internet,
  giống hệt khi chạy trên runner CI bình thường. Repo không cần tải gì thì đặt `REPOGUARD_EXECUTOR_NETWORK=none`.
- Không có secret nào trong container, nên test cần database hay API key thật sẽ không chạy được.
  Đây là chủ ý.

## Cài đặt

1. Chuẩn bị một VM Linux có Docker (khuyến nghị cài thêm [gVisor](https://gvisor.dev/docs/user_guide/install/)).
2. Điền `deploy/.env` (từ `.env.example`): phần GitLab, cộng thêm:
   ```bash
   REPOGUARD_EXECUTOR=docker
   REPOGUARD_EXECUTOR_REPOS=nhom-cua-ban/*        # chỉ các repo này được build
   # REPOGUARD_EXECUTOR_RUNTIME=runsc              # nếu đã cài gVisor
   ```
3. Chạy:
   ```bash
   docker pull ghcr.io/dpvinh30092005/repoguard-ci:1     # image dùng để build
   docker compose -f deploy/docker-compose.executor.yml up -d
   ```
4. Mở thử một MR trong repo được phép. Sau vài phút, MR sẽ có comment **🧪 repoguard ci · commit …**
   chứa bảng lint/build/test của từng project.

## Cấu hình

| Biến | Mặc định | Ý nghĩa |
|---|---|---|
| `REPOGUARD_EXECUTOR` | (tắt) | `docker` để bật |
| `REPOGUARD_EXECUTOR_REPOS` | (bắt buộc) | danh sách glob theo đường dẫn repo, phân cách bằng dấu phẩy |
| `REPOGUARD_EXECUTOR_IMAGE` | `ghcr.io/dpvinh30092005/repoguard-ci:1` | image chứa Go, Node, Python, osv-scanner, repoguard |
| `REPOGUARD_EXECUTOR_CPUS` / `_MEMORY` | `2` / `4g` | giới hạn mỗi lần build |
| `REPOGUARD_EXECUTOR_TIMEOUT` | `20m` | quá thời gian thì container bị kill và MR báo "Timed out" |
| `REPOGUARD_EXECUTOR_CONCURRENCY` | `2` | số build chạy song song |
| `REPOGUARD_EXECUTOR_PIDS_LIMIT` | `1024` | chặn fork bomb |
| `REPOGUARD_EXECUTOR_NETWORK` | `bridge` | `none` để cắt mạng hoàn toàn |
| `REPOGUARD_EXECUTOR_RUNTIME` | | ví dụ `runsc` (gVisor) |

Lệnh build của từng project lấy từ `.repoguard.yml` (mục `ci:`) trên default branch, hoặc tự nhận
theo `go.mod` / `package.json` / `pyproject.toml`. Xem `.repoguard.example.yml`.

## Khi pipeline bị tắt, những gì khác đi

- **Commit status:** khi CI/CD bị tắt, GitLab có thể từ chối API commit status (lỗi 403). repoguard
  sẽ bỏ qua status và đưa toàn bộ kết quả vào comment trên MR. Hãy thử trên một MR để biết GitLab
  của bạn thuộc trường hợp nào.
- **Chặn merge:** không có pipeline thì không dùng được *Pipelines must succeed*. Muốn chặn merge,
  cần tính năng trả phí của GitLab (*External status checks*, bản Ultimate) hoặc quy ước review
  "không merge khi comment repoguard báo ❌".
- **Phạm vi build:** executor build toàn bộ project trong repo cho mỗi commit (lấy shallow clone nên
  không so được với nhánh đích). Repo lớn thì tăng timeout hoặc khai báo `ci.projects`.
