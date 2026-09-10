package main

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// ---- 高可用:租约、代次、围栏、跨实例幂等 ----

// 双实例(多实例)同时争抢接管:恰好一个获胜,代次只 +1
func TestTakeoverRaceExactlyOneWinner(t *testing.T) {
	resetState(t)
	forceLease(t, "dead-instance", -time.Second, 5) // 前任已死,租约过期

	const n = 8
	var wins int64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, acquired, err := testServer.store.AcquireLease(context.Background(),
				fmt.Sprintf("contender-%d", i), "race test", time.Minute, time.Now())
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			if acquired {
				atomic.AddInt64(&wins, 1)
			}
		}(i)
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("winners = %d, want exactly 1", wins)
	}
	lease, err := testServer.store.LeaseState(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if lease.Epoch != 6 {
		t.Fatalf("epoch = %d, want 6 (恰好递增一次)", lease.Epoch)
	}
	history, err := testServer.store.TakeoverHistory(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 {
		t.Fatalf("history rows = %d, want 1", len(history))
	}
}

// 非权威实例(从未持有/已被接管)一律被围栏拒绝签发
func TestNonAuthoritativeCannotIssue(t *testing.T) {
	resetState(t)
	poolID, deptID := seedFixture(t, 10, 2)
	routerB := newInstanceRouter("instance-b")

	// B 从未持有租约
	r := borrowOn(t, routerB, poolID, deptID, "乙", "online", "ha-fence-1", 0)
	if r.code != http.StatusConflict || r.body["error"] != "not_authoritative" {
		t.Fatalf("standby borrow: %d %v", r.code, r.body)
	}
	if n := activeCount(t, poolID, deptID); n != 0 {
		t.Fatalf("active = %d, want 0 (被围栏拒绝不得产生记录)", n)
	}
}

// 旧实例恢复后不得继续签发:本地认知过期,数据库围栏兜底
func TestOldInstanceRecoveryCannotIssue(t *testing.T) {
	resetState(t) // test-instance 持有,epoch 1
	poolID, deptID := seedFixture(t, 10, 2)
	routerB := newInstanceRouter("instance-b")

	// 旧实例正常签发
	if r := borrowOn(t, testRouter, poolID, deptID, "甲", "online", "ha-rec-1", 0); r.code != http.StatusCreated {
		t.Fatalf("borrow before failover: %d %v", r.code, r.body)
	}

	// 故障转移:租约过期,B 接管,epoch 2
	forceLease(t, "test-instance", -time.Second, 1)
	lease, acquired, err := testServer.store.AcquireLease(context.Background(), "instance-b", "租约过期接管", time.Minute, time.Now())
	if err != nil || !acquired {
		t.Fatalf("takeover: acquired=%v err=%v", acquired, err)
	}
	if lease.Epoch != 2 {
		t.Fatalf("epoch = %d, want 2", lease.Epoch)
	}

	// 旧实例"恢复",本地仍自认权威,但签发被数据库围栏拦下
	r := borrowOn(t, testRouter, poolID, deptID, "甲", "online", "ha-rec-2", 0)
	if r.code != http.StatusConflict || r.body["error"] != "not_authoritative" {
		t.Fatalf("recovered old instance borrow: %d %v", r.code, r.body)
	}
	leaseInfo := r.body["lease"].(map[string]any)
	if leaseInfo["holder_id"] != "instance-b" {
		t.Fatalf("response should reveal current holder: %v", leaseInfo)
	}

	// 旧实例续租也会失败(双重匹配 holder+epoch)
	ok, err := testServer.store.RenewLease(context.Background(), "test-instance", 1, time.Minute, time.Now())
	if err != nil || ok {
		t.Fatalf("old instance renew: ok=%v err=%v, want false", ok, err)
	}

	// 新权威可以签发,且代次为 2
	r = borrowOn(t, routerB, poolID, deptID, "乙", "online", "ha-rec-3", 0)
	if r.code != http.StatusCreated {
		t.Fatalf("new authority borrow: %d %v", r.code, r.body)
	}
	b := r.body["borrow"].(map[string]any)
	if b["issued_epoch"].(float64) != 2 {
		t.Fatalf("issued_epoch = %v, want 2", b["issued_epoch"])
	}
}

// 故障转移后,已有离线借用仍按原有效期占用额度
func TestOfflineBorrowOccupiesAcrossTakeover(t *testing.T) {
	resetState(t)
	poolID, deptID := seedFixture(t, 10, 1) // 部门额度 1
	routerB := newInstanceRouter("instance-b")

	r := borrowOn(t, testRouter, poolID, deptID, "离线员工", "offline", "ha-off-1", 300)
	if r.code != http.StatusCreated {
		t.Fatalf("offline borrow: %d %v", r.code, r.body)
	}
	cred := r.body["credential"].(string)
	claims, err := ParseCredential([]byte("test-secret"), cred)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Epoch != 1 {
		t.Fatalf("credential epoch = %d, want 1", claims.Epoch)
	}

	// 接管:epoch 1 -> 2。离线借用不得因代次变化被释放
	forceLease(t, "test-instance", -time.Second, 1)
	if _, acquired, _ := testServer.store.AcquireLease(context.Background(), "instance-b", "takeover", time.Minute, time.Now()); !acquired {
		t.Fatal("takeover failed")
	}
	if n := activeCount(t, poolID, deptID); n != 1 {
		t.Fatalf("active = %d after takeover, want 1 (离线借用仍占用)", n)
	}

	// 新实例上同部门新借用仍被该离线席位挡住
	r = borrowOn(t, routerB, poolID, deptID, "新申请", "online", "ha-off-2", 0)
	if r.code != http.StatusConflict || r.body["error"] != "dept_quota_exhausted" {
		t.Fatalf("borrow on new instance: %d %v", r.code, r.body)
	}
}

// 旧代次凭证交给新实例:可验证、可归还、幂等
func TestOldCredentialReturnAfterTakeover(t *testing.T) {
	resetState(t)
	poolID, deptID := seedFixture(t, 10, 1)
	routerB := newInstanceRouter("instance-b")

	r := borrowOn(t, testRouter, poolID, deptID, "离线员工", "offline", "ha-ret-1", 300)
	cred := r.body["credential"].(string) // epoch 1 签发

	forceLease(t, "test-instance", -time.Second, 1)
	if _, acquired, _ := testServer.store.AcquireLease(context.Background(), "instance-b", "takeover", time.Minute, time.Now()); !acquired {
		t.Fatal("takeover failed")
	}

	// 旧凭证归还给新实例
	rr := doJSONOn(t, routerB, http.MethodPost, "/api/returns", map[string]any{"credential": cred})
	if rr.code != http.StatusOK || rr.body["result"] != "returned" {
		t.Fatalf("return old credential to new instance: %d %v", rr.code, rr.body)
	}
	b := rr.body["borrow"].(map[string]any)
	if b["issued_epoch"].(float64) != 1 {
		t.Fatalf("issued_epoch = %v, want 1 (原代次保留)", b["issued_epoch"])
	}
	// 幂等重复归还
	rr = doJSONOn(t, routerB, http.MethodPost, "/api/returns", map[string]any{"credential": cred})
	if rr.code != http.StatusOK || rr.body["result"] != "already_closed" {
		t.Fatalf("repeat return: %d %v", rr.code, rr.body)
	}
	if n := activeCount(t, poolID, deptID); n != 0 {
		t.Fatalf("active = %d, want 0", n)
	}
}

// 签发提交后响应丢失:客户端持同一幂等键向新实例重试,拿到原借用结果
func TestResponseLostRetryAcrossInstance(t *testing.T) {
	resetState(t)
	poolID, deptID := seedFixture(t, 10, 2)
	routerB := newInstanceRouter("instance-b")

	// A 提交并落库,但响应在客户端视角"丢失"(实际已提交)
	r1 := borrowOn(t, testRouter, poolID, deptID, "张三", "offline", "ha-lost-1", 120)
	if r1.code != http.StatusCreated {
		t.Fatalf("first borrow: %d %v", r1.code, r1.body)
	}
	id1 := borrowIDOf(t, r1)
	cred1 := r1.body["credential"].(string)

	// A 故障,B 接管
	forceLease(t, "test-instance", -time.Second, 1)
	if _, acquired, _ := testServer.store.AcquireLease(context.Background(), "instance-b", "takeover", time.Minute, time.Now()); !acquired {
		t.Fatal("takeover failed")
	}

	// 客户端持同一幂等键向新实例 B 重试:返回原记录,凭证逐字节相同
	r2 := borrowOn(t, routerB, poolID, deptID, "张三", "offline", "ha-lost-1", 120)
	if r2.code != http.StatusOK || r2.body["replay"] != true {
		t.Fatalf("retry on new instance: %d %v", r2.code, r2.body)
	}
	if borrowIDOf(t, r2) != id1 {
		t.Fatalf("retry got different id")
	}
	if r2.body["credential"].(string) != cred1 {
		t.Fatalf("replayed credential must be byte-identical to the lost one")
	}
	if n := activeCount(t, poolID, deptID); n != 1 {
		t.Fatalf("active = %d, want 1 (重试不得产生新记录)", n)
	}
}

// 部门迁移:只改归属,不制造新可用席位
func TestDepartmentMigration(t *testing.T) {
	resetState(t)
	poolID, dept1 := seedFixture(t, 10, 1)
	res, err := testDB.Exec(`INSERT INTO departments (name) VALUES (?)`, "迁移目标-"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	dept2, _ := res.LastInsertId()
	routerB := newInstanceRouter("instance-b")

	r := borrowOn(t, testRouter, poolID, dept1, "迁移员工", "online", "ha-mig-1", 0)
	id := borrowIDOf(t, r)

	// 迁移:dept1 -> dept2
	m := doJSONOn(t, testRouter, http.MethodPost, fmt.Sprintf("/api/borrows/%d/migrate", id),
		map[string]any{"department_id": dept2})
	if m.code != http.StatusOK || m.body["result"] != "migrated" {
		t.Fatalf("migrate: %d %v", m.code, m.body)
	}
	if m.body["target_active"].(float64) != 1 {
		t.Fatalf("target_active = %v, want 1", m.body["target_active"])
	}

	// 池级占用不变;dept2 没有额度行,迁移过来的席位不制造新可借席位
	ov := doJSONOn(t, testRouter, http.MethodGet, "/api/overview", nil)
	pools := ov.body["pools"].([]any)
	var active float64
	for _, p := range pools {
		pm := p.(map[string]any)
		if int64(pm["id"].(float64)) == poolID {
			active = pm["active"].(float64)
		}
	}
	if active != 1 {
		t.Fatalf("pool active = %v, want 1 (迁移不得改变池占用)", active)
	}
	r = borrowOn(t, testRouter, poolID, dept2, "蹭席位", "online", "ha-mig-2", 0)
	if r.code != http.StatusConflict || r.body["error"] != "no_quota" {
		t.Fatalf("borrow into quota-less dept after migration: %d %v", r.code, r.body)
	}

	// 已归还的借用不能迁移
	doJSONOn(t, testRouter, http.MethodPost, "/api/returns", map[string]any{"borrow_id": id})
	m = doJSONOn(t, testRouter, http.MethodPost, fmt.Sprintf("/api/borrows/%d/migrate", id),
		map[string]any{"department_id": dept1})
	if m.code != http.StatusConflict || m.body["error"] != "borrow_not_active" {
		t.Fatalf("migrate closed borrow: %d %v", m.code, m.body)
	}

	// 非权威实例不能迁移
	r = borrowOn(t, testRouter, poolID, dept1, "第二个", "online", "ha-mig-3", 0)
	id2 := borrowIDOf(t, r)
	forceLease(t, "test-instance", -time.Second, 1)
	if _, acquired, _ := testServer.store.AcquireLease(context.Background(), "instance-b", "takeover", time.Minute, time.Now()); !acquired {
		t.Fatal("takeover failed")
	}
	m = doJSONOn(t, testRouter, http.MethodPost, fmt.Sprintf("/api/borrows/%d/migrate", id2),
		map[string]any{"department_id": dept2})
	if m.code != http.StatusConflict || m.body["error"] != "not_authoritative" {
		t.Fatalf("migrate on fenced instance: %d %v", m.code, m.body)
	}
	// 新权威可以迁移
	m = doJSONOn(t, routerB, http.MethodPost, fmt.Sprintf("/api/borrows/%d/migrate", id2),
		map[string]any{"department_id": dept2})
	if m.code != http.StatusOK {
		t.Fatalf("migrate on new authority: %d %v", m.code, m.body)
	}
}

// 不变量:任何时刻有效占用不超过池容量(含接管过程的争抢)
func TestCapacityInvariantUnderContention(t *testing.T) {
	resetState(t)
	poolID, deptID := seedFixture(t, 2, 2) // 池容量 2,部门额度 2
	routerB := newInstanceRouter("instance-b")

	var created int64
	fire := func(router *gin.Engine, prefix string, n int) {
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				r := borrowOn(t, router, poolID, deptID, "员工", "online",
					fmt.Sprintf("%s-%d", prefix, i), 0)
				if r.code == http.StatusCreated {
					atomic.AddInt64(&created, 1)
				}
			}(i)
		}
		wg.Wait()
	}

	// 阶段 1:A 权威。A、B 同时借用,B 全部被围栏拒
	fire(testRouter, "cap-a", 4)
	fire(routerB, "cap-b-fenced", 4)
	// 阶段 2:B 接管
	forceLease(t, "test-instance", -time.Second, 1)
	if _, acquired, _ := testServer.store.AcquireLease(context.Background(), "instance-b", "takeover", time.Minute, time.Now()); !acquired {
		t.Fatal("takeover failed")
	}
	// 阶段 3:B 继续签发
	fire(routerB, "cap-b-auth", 4)

	if created > 2 {
		t.Fatalf("created = %d, 超过池容量 2", created)
	}
	if n := activeCount(t, poolID, deptID); n != int(created) || n > 2 {
		t.Fatalf("active = %d (created %d), 必须一致且不超过容量 2", n, created)
	}
}

// 权威状态接口:数据库租约是唯一事实
func TestAuthorityEndpoint(t *testing.T) {
	resetState(t)
	r := doJSONOn(t, testRouter, http.MethodGet, "/api/authority", nil)
	if r.code != http.StatusOK {
		t.Fatalf("authority: %d", r.code)
	}
	if r.body["instance_id"] != "test-instance" || r.body["is_authoritative"] != true {
		t.Fatalf("authority body: %v", r.body)
	}
	lease := r.body["lease"].(map[string]any)
	if lease["holder_id"] != "test-instance" || lease["valid"] != true {
		t.Fatalf("lease: %v", lease)
	}

	// 租约被别人拿走后,本实例不再权威
	forceLease(t, "someone-else", time.Hour, 9)
	r = doJSONOn(t, testRouter, http.MethodGet, "/api/authority", nil)
	if r.body["is_authoritative"] != false {
		t.Fatalf("should not be authoritative: %v", r.body)
	}
	lease = r.body["lease"].(map[string]any)
	if lease["epoch"].(float64) != 9 {
		t.Fatalf("epoch = %v, want 9", lease["epoch"])
	}
}
