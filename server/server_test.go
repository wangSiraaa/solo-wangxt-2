package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

var (
	testDB     *sql.DB
	testServer *Server
	testRouter *gin.Engine
)

func TestMain(m *testing.M) {
	adminDSN := envOr("TEST_ADMIN_DSN", "app:app123@tcp(127.0.0.1:3306)/?parseTime=true&multiStatements=true")
	admin, err := sql.Open("mysql", adminDSN)
	if err != nil {
		panic(err)
	}
	if _, err := admin.Exec(`CREATE DATABASE IF NOT EXISTS licensehub_test CHARACTER SET utf8mb4`); err != nil {
		panic(fmt.Sprintf("create test db: %v", err))
	}
	admin.Close()

	dsn := envOr("TEST_DB_DSN", "app:app123@tcp(127.0.0.1:3306)/licensehub_test?parseTime=true&multiStatements=true")
	testDB, err = sql.Open("mysql", dsn)
	if err != nil {
		panic(err)
	}
	if err := testDB.Ping(); err != nil {
		panic(fmt.Sprintf("ping test db: %v", err))
	}

	schema, err := os.ReadFile("schema.sql")
	if err != nil {
		panic(err)
	}
	if _, err := testDB.Exec(string(schema)); err != nil {
		panic(fmt.Sprintf("apply schema: %v", err))
	}

	gin.SetMode(gin.TestMode)
	cache := NewCache(envOr("TEST_REDIS_ADDR", "127.0.0.1:6379"), 1) // 独立 db 号,避免污染演示数据
	testServer = &Server{store: NewStore(testDB), cache: cache, cred: []byte("test-secret"), now: time.Now}
	testRouter = NewRouter(testServer)

	os.Exit(m.Run())
}

func resetState(t *testing.T) {
	t.Helper()
	for _, table := range []string{"borrows", "department_quotas", "license_pools", "departments"} {
		if _, err := testDB.Exec("DELETE FROM " + table); err != nil {
			t.Fatalf("reset %s: %v", table, err)
		}
	}
	if testServer.cache.Enabled() {
		testServer.cache.rdb.FlushDB(context.Background())
	}
}

// seedFixture 建一个部门、一个池、一条额度
func seedFixture(t *testing.T, totalSeats, quota int) (poolID, deptID int64) {
	t.Helper()
	r1, err := testDB.Exec(`INSERT INTO departments (name) VALUES (?)`, fmt.Sprintf("部门-%s", t.Name()))
	if err != nil {
		t.Fatal(err)
	}
	deptID, _ = r1.LastInsertId()
	r2, err := testDB.Exec(`INSERT INTO license_pools (product, total_seats) VALUES (?, ?)`,
		fmt.Sprintf("Pool-%s", t.Name()), totalSeats)
	if err != nil {
		t.Fatal(err)
	}
	poolID, _ = r2.LastInsertId()
	if _, err := testDB.Exec(
		`INSERT INTO department_quotas (pool_id, department_id, quota, updated_by) VALUES (?,?,?,'test')`,
		poolID, deptID, quota); err != nil {
		t.Fatal(err)
	}
	return poolID, deptID
}

// ---- HTTP 辅助 ----

type apiResp struct {
	code   int
	body   map[string]any
	header http.Header
}

func doJSON(t *testing.T, method, path string, payload any) apiResp {
	t.Helper()
	var buf bytes.Buffer
	if payload != nil {
		if err := json.NewEncoder(&buf).Encode(payload); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	testRouter.ServeHTTP(w, req)
	res := apiResp{code: w.Code, header: w.Header()}
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &res.body); err != nil {
			t.Fatalf("decode response %s %s: %v\n%s", method, path, err, w.Body.String())
		}
	}
	return res
}

func borrowOnce(t *testing.T, poolID, deptID int64, employee, mode, key string, ttl int) apiResp {
	return doJSON(t, http.MethodPost, "/api/borrows", map[string]any{
		"pool_id": poolID, "department_id": deptID, "employee": employee,
		"mode": mode, "ttl_seconds": ttl, "idempotency_key": key,
	})
}

func activeCount(t *testing.T, poolID, deptID int64) int {
	t.Helper()
	var n int
	if err := testDB.QueryRow(
		`SELECT COUNT(*) FROM borrows WHERE pool_id=? AND department_id=? AND status='active'`,
		poolID, deptID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func borrowIDOf(t *testing.T, r apiResp) int64 {
	t.Helper()
	b, ok := r.body["borrow"].(map[string]any)
	if !ok {
		t.Fatalf("response missing borrow: %v", r.body)
	}
	return int64(b["id"].(float64))
}

// ---- 测试 ----

// 幂等:同一幂等键重试返回原借用结果
func TestBorrowIdempotentReplay(t *testing.T) {
	resetState(t)
	poolID, deptID := seedFixture(t, 10, 2)

	r1 := borrowOnce(t, poolID, deptID, "张三", "online", "key-replay-001", 0)
	if r1.code != http.StatusCreated {
		t.Fatalf("first borrow: got %d %v", r1.code, r1.body)
	}
	id1 := borrowIDOf(t, r1)

	r2 := borrowOnce(t, poolID, deptID, "张三", "online", "key-replay-001", 0)
	if r2.code != http.StatusOK || r2.body["replay"] != true {
		t.Fatalf("replay: got %d %v", r2.code, r2.body)
	}
	if borrowIDOf(t, r2) != id1 {
		t.Fatalf("replay returned different borrow id")
	}
	if n := activeCount(t, poolID, deptID); n != 1 {
		t.Fatalf("active = %d, want 1 (重试不得产生第二条记录)", n)
	}
}

// 并发同一幂等键:只允许落一条记录,所有请求拿到同一个借用 ID
func TestConcurrentSameIdempotencyKey(t *testing.T) {
	resetState(t)
	poolID, deptID := seedFixture(t, 10, 5)

	const n = 6
	ids := make([]int64, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := borrowOnce(t, poolID, deptID, "李四", "online", "key-same-001", 0)
			if r.code != http.StatusCreated && r.code != http.StatusOK {
				t.Errorf("unexpected code %d: %v", r.code, r.body)
				return
			}
			ids[i] = borrowIDOf(t, r)
		}(i)
	}
	wg.Wait()
	for i := 1; i < n; i++ {
		if ids[i] != ids[0] {
			t.Fatalf("concurrent same-key got different ids: %v", ids)
		}
	}
	if got := activeCount(t, poolID, deptID); got != 1 {
		t.Fatalf("active = %d, want 1", got)
	}
}

// 最后一个席位的并发竞争:只能一人成功
func TestConcurrentLastSeat(t *testing.T) {
	resetState(t)
	poolID, deptID := seedFixture(t, 10, 1) // 部门额度 = 1

	const n = 8
	var created, conflict int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := borrowOnce(t, poolID, deptID, fmt.Sprintf("员工%d", i), "online",
				fmt.Sprintf("key-race-%d", i), 0)
			mu.Lock()
			switch r.code {
			case http.StatusCreated:
				created++
			case http.StatusConflict:
				conflict++
			default:
				t.Errorf("unexpected %d: %v", r.code, r.body)
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	if created != 1 || conflict != n-1 {
		t.Fatalf("created=%d conflict=%d, want 1/%d", created, conflict, n-1)
	}
	if got := activeCount(t, poolID, deptID); got != 1 {
		t.Fatalf("active = %d, want 1", got)
	}
}

// 池总席位同样构成硬上限
func TestPoolTotalCap(t *testing.T) {
	resetState(t)
	poolID, deptID := seedFixture(t, 1, 5)

	if r := borrowOnce(t, poolID, deptID, "甲", "online", "key-cap-1", 0); r.code != http.StatusCreated {
		t.Fatalf("first: %d %v", r.code, r.body)
	}
	r := borrowOnce(t, poolID, deptID, "乙", "online", "key-cap-2", 0)
	if r.code != http.StatusConflict || r.body["error"] != "pool_exhausted" {
		t.Fatalf("second: %d %v", r.code, r.body)
	}
}

// 离线借用 + 有效凭证提前归还
func TestOfflineBorrowAndEarlyReturn(t *testing.T) {
	resetState(t)
	poolID, deptID := seedFixture(t, 10, 1)

	r := borrowOnce(t, poolID, deptID, "王五", "offline", "key-off-1", 60)
	if r.code != http.StatusCreated {
		t.Fatalf("borrow: %d %v", r.code, r.body)
	}
	cred, _ := r.body["credential"].(string)
	if cred == "" {
		t.Fatalf("offline borrow must return credential: %v", r.body)
	}

	rr := doJSON(t, http.MethodPost, "/api/returns", map[string]any{"credential": cred})
	if rr.code != http.StatusOK || rr.body["result"] != "returned" {
		t.Fatalf("early return: %d %v", rr.code, rr.body)
	}
	if n := activeCount(t, poolID, deptID); n != 0 {
		t.Fatalf("active = %d, want 0", n)
	}
	// 凭证重复使用:幂等,不报错也不产生副作用
	rr2 := doJSON(t, http.MethodPost, "/api/returns", map[string]any{"credential": cred})
	if rr2.code != http.StatusOK || rr2.body["result"] != "already_closed" {
		t.Fatalf("repeat return: %d %v", rr2.code, rr2.body)
	}
}

// 离线席位到期前:无凭证/伪造凭证一律不能释放
func TestOfflineReturnRequiresValidCredential(t *testing.T) {
	resetState(t)
	poolID, deptID := seedFixture(t, 10, 1)

	r := borrowOnce(t, poolID, deptID, "赵六", "offline", "key-off-2", 60)
	id := borrowIDOf(t, r)

	// 无凭证
	rr := doJSON(t, http.MethodPost, "/api/returns", map[string]any{"borrow_id": id})
	if rr.code != http.StatusForbidden || rr.body["error"] != "credential_required" {
		t.Fatalf("no credential: %d %v", rr.code, rr.body)
	}
	// 格式非法
	rr = doJSON(t, http.MethodPost, "/api/returns", map[string]any{"credential": "garbage"})
	if rr.code != http.StatusUnauthorized {
		t.Fatalf("malformed: %d %v", rr.code, rr.body)
	}
	// 格式合法但签名错误(用别的密钥签)
	forged := SignCredential([]byte("wrong-secret"), Claims{BorrowID: id, PoolID: poolID, Nonce: "x", ExpUnixMs: time.Now().Add(time.Hour).UnixMilli()})
	rr = doJSON(t, http.MethodPost, "/api/returns", map[string]any{"credential": forged})
	if rr.code != http.StatusUnauthorized {
		t.Fatalf("forged: %d %v", rr.code, rr.body)
	}
	// 签名正确但 nonce 与借用不匹配
	mismatched := SignCredential([]byte("test-secret"), Claims{BorrowID: id, PoolID: poolID, Nonce: "not-the-nonce", ExpUnixMs: time.Now().Add(time.Hour).UnixMilli()})
	rr = doJSON(t, http.MethodPost, "/api/returns", map[string]any{"credential": mismatched})
	if rr.code != http.StatusUnauthorized {
		t.Fatalf("nonce mismatch: %d %v", rr.code, rr.body)
	}
	if n := activeCount(t, poolID, deptID); n != 1 {
		t.Fatalf("active = %d, want 1 (席位不得被释放)", n)
	}
}

// 在线席位直接归还;重复归还幂等
func TestOnlineReturn(t *testing.T) {
	resetState(t)
	poolID, deptID := seedFixture(t, 10, 2)

	r := borrowOnce(t, poolID, deptID, "孙七", "online", "key-on-1", 0)
	id := borrowIDOf(t, r)

	rr := doJSON(t, http.MethodPost, "/api/returns", map[string]any{"borrow_id": id})
	if rr.code != http.StatusOK || rr.body["result"] != "returned" {
		t.Fatalf("return: %d %v", rr.code, rr.body)
	}
	rr = doJSON(t, http.MethodPost, "/api/returns", map[string]any{"borrow_id": id})
	if rr.code != http.StatusOK || rr.body["result"] != "already_closed" {
		t.Fatalf("repeat return: %d %v", rr.code, rr.body)
	}
}

// 凭证过期但回收器未跑:不允许主动归还,席位仍占用
func TestExpiredCredentialReturnBeforeReclaim(t *testing.T) {
	resetState(t)
	poolID, deptID := seedFixture(t, 10, 1)

	r := borrowOnce(t, poolID, deptID, "周八", "offline", "key-exp-1", 1)
	cred := r.body["credential"].(string)
	time.Sleep(1200 * time.Millisecond)

	rr := doJSON(t, http.MethodPost, "/api/returns", map[string]any{"credential": cred})
	if rr.code != http.StatusGone || rr.body["error"] != "credential_expired" {
		t.Fatalf("expired return: %d %v", rr.code, rr.body)
	}
	if n := activeCount(t, poolID, deptID); n != 1 {
		t.Fatalf("active = %d, want 1 (未回收前仍占用)", n)
	}
}

// 核心场景:过期凭证重复归还。
// 借用 -> 过期 -> 回收 -> 过期凭证归还两次:两次都返回确定的 already_closed,
// 席位只被释放一次(额度=1 的池子恰好能再借出一个,第二个仍被拒绝)。
func TestExpiredCredentialDuplicateReturn(t *testing.T) {
	resetState(t)
	poolID, deptID := seedFixture(t, 10, 1)

	r := borrowOnce(t, poolID, deptID, "吴九", "offline", "key-dup-1", 1)
	if r.code != http.StatusCreated {
		t.Fatalf("borrow: %d %v", r.code, r.body)
	}
	cred := r.body["credential"].(string)
	time.Sleep(1200 * time.Millisecond)

	reclaim := doJSON(t, http.MethodPost, "/api/admin/reclaim", nil)
	if reclaim.body["reclaimed"].(float64) != 1 {
		t.Fatalf("reclaim: %v", reclaim.body)
	}

	for i := 0; i < 2; i++ {
		rr := doJSON(t, http.MethodPost, "/api/returns", map[string]any{"credential": cred})
		if rr.code != http.StatusOK || rr.body["result"] != "already_closed" {
			t.Fatalf("expired duplicate return #%d: %d %v", i+1, rr.code, rr.body)
		}
		b := rr.body["borrow"].(map[string]any)
		if b["status"] != "reclaimed" {
			t.Fatalf("status = %v, want reclaimed", b["status"])
		}
	}

	if n := activeCount(t, poolID, deptID); n != 0 {
		t.Fatalf("active = %d, want 0", n)
	}
	// 席位恰好释放一次:可以再借一个,但不能再多
	if r := borrowOnce(t, poolID, deptID, "郑十", "online", "key-dup-2", 0); r.code != http.StatusCreated {
		t.Fatalf("re-borrow after reclaim: %d %v", r.code, r.body)
	}
	if r := borrowOnce(t, poolID, deptID, "再借", "online", "key-dup-3", 0); r.code != http.StatusConflict {
		t.Fatalf("seat must not be double-freed: %d %v", r.code, r.body)
	}
}

// 回收器只收到期的,未到期离线席位不受影响
func TestReclaimOnlyExpired(t *testing.T) {
	resetState(t)
	poolID, deptID := seedFixture(t, 10, 2)

	borrowOnce(t, poolID, deptID, "长借", "offline", "key-reap-1", 3600)
	borrowOnce(t, poolID, deptID, "短借", "offline", "key-reap-2", 1)
	time.Sleep(1200 * time.Millisecond)

	r := doJSON(t, http.MethodPost, "/api/admin/reclaim", nil)
	if r.body["reclaimed"].(float64) != 1 {
		t.Fatalf("reclaim: %v", r.body)
	}
	if n := activeCount(t, poolID, deptID); n != 1 {
		t.Fatalf("active = %d, want 1 (未到期保留)", n)
	}
}

// 缩减额度:现存借用保留,只限制新申请
func TestQuotaReductionKeepsExistingBorrows(t *testing.T) {
	resetState(t)
	poolID, deptID := seedFixture(t, 10, 2)

	b1 := borrowOnce(t, poolID, deptID, "员工A", "online", "key-quota-1", 0)
	borrowOnce(t, poolID, deptID, "员工B", "online", "key-quota-2", 0)
	idA := borrowIDOf(t, b1)

	// 额度 2 -> 1:现存 2 个借用不动
	r := doJSON(t, http.MethodPost, "/api/quotas", map[string]any{
		"pool_id": poolID, "department_id": deptID, "quota": 1, "adjusted_by": "部门负责人"})
	if r.code != http.StatusOK {
		t.Fatalf("adjust: %d %v", r.code, r.body)
	}
	if r.body["active_borrows"].(float64) != 2 || r.body["note"] == nil {
		t.Fatalf("adjust resp: %v", r.body)
	}
	if n := activeCount(t, poolID, deptID); n != 2 {
		t.Fatalf("active = %d, want 2 (缩减不得抹掉现存借用)", n)
	}

	// 新申请被拒
	if r := borrowOnce(t, poolID, deptID, "员工C", "online", "key-quota-3", 0); r.code != http.StatusConflict {
		t.Fatalf("new borrow over quota: %d %v", r.code, r.body)
	}
	// 归还一个后占用=1,仍 >= 新额度 1,继续拒
	doJSON(t, http.MethodPost, "/api/returns", map[string]any{"borrow_id": idA})
	r2 := borrowOnce(t, poolID, deptID, "员工C", "online", "key-quota-4", 0)
	if r2.code != http.StatusConflict {
		t.Fatalf("borrow at quota boundary: %d %v", r2.code, r2.body)
	}
	// 调回 2 后立即可借
	doJSON(t, http.MethodPost, "/api/quotas", map[string]any{
		"pool_id": poolID, "department_id": deptID, "quota": 2, "adjusted_by": "部门负责人"})
	if r := borrowOnce(t, poolID, deptID, "员工C", "online", "key-quota-5", 0); r.code != http.StatusCreated {
		t.Fatalf("borrow after quota raise: %d %v", r.code, r.body)
	}
}

// 额度降到 0:现存保留,新申请全拒
func TestQuotaReductionToZero(t *testing.T) {
	resetState(t)
	poolID, deptID := seedFixture(t, 10, 1)

	borrowOnce(t, poolID, deptID, "员工A", "online", "key-zero-1", 0)
	doJSON(t, http.MethodPost, "/api/quotas", map[string]any{
		"pool_id": poolID, "department_id": deptID, "quota": 0, "adjusted_by": "部门负责人"})

	if n := activeCount(t, poolID, deptID); n != 1 {
		t.Fatalf("active = %d, want 1", n)
	}
	if r := borrowOnce(t, poolID, deptID, "员工B", "online", "key-zero-2", 0); r.code != http.StatusConflict {
		t.Fatalf("borrow with zero quota: %d %v", r.code, r.body)
	}
}

// 部门在池中没有额度行 -> 409 no_quota
func TestNoQuotaRow(t *testing.T) {
	resetState(t)
	poolID, _ := seedFixture(t, 10, 1)
	r, err := testDB.Exec(`INSERT INTO departments (name) VALUES (?)`, "无额度部门-"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	otherDept, _ := r.LastInsertId()

	resp := borrowOnce(t, poolID, otherDept, "员工X", "online", "key-nq-1", 0)
	if resp.code != http.StatusConflict || resp.body["error"] != "no_quota" {
		t.Fatalf("no quota: %d %v", resp.code, resp.body)
	}
}

// Redis 仅缓存查询:第一次 MISS,第二次 HIT,写操作后失效再 MISS
func TestOverviewCacheAside(t *testing.T) {
	resetState(t)
	if !testServer.cache.Enabled() {
		t.Skip("redis disabled")
	}
	poolID, deptID := seedFixture(t, 10, 2)

	r1 := doJSON(t, http.MethodGet, "/api/overview", nil)
	if r1.header.Get("X-Cache") != "MISS" {
		t.Fatalf("first overview should MISS, got %q", r1.header.Get("X-Cache"))
	}
	r2 := doJSON(t, http.MethodGet, "/api/overview", nil)
	if r2.header.Get("X-Cache") != "HIT" {
		t.Fatalf("second overview should HIT, got %q", r2.header.Get("X-Cache"))
	}
	borrowOnce(t, poolID, deptID, "缓存测试", "online", "key-cache-1", 0)
	r3 := doJSON(t, http.MethodGet, "/api/overview", nil)
	if r3.header.Get("X-Cache") != "MISS" {
		t.Fatalf("overview after write should MISS (invalidated), got %q", r3.header.Get("X-Cache"))
	}
}
