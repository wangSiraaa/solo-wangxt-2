package main

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/go-sql-driver/mysql"
)

// 业务错误,由 HTTP 层映射为状态码
var (
	ErrPoolNotFound       = errors.New("pool_not_found")
	ErrNoQuota            = errors.New("no_quota")             // 该部门在此池没有额度
	ErrDeptQuotaExhausted = errors.New("dept_quota_exhausted") // 部门额度已占满
	ErrPoolExhausted      = errors.New("pool_exhausted")       // 池总席位已占满
	ErrBorrowNotFound     = errors.New("borrow_not_found")
	ErrNotAuthoritative   = errors.New("not_authoritative") // 本实例不持有有效租约,禁止签发
	ErrBorrowNotActive    = errors.New("borrow_not_active")
	ErrDepartmentNotFound = errors.New("department_not_found")
)

const (
	ModeOnline  = "online"
	ModeOffline = "offline"

	StatusActive    = "active"
	StatusReturned  = "returned"
	StatusReclaimed = "reclaimed"
)

type Borrow struct {
	ID               int64      `json:"id"`
	IdempotencyKey   string     `json:"idempotency_key"`
	PoolID           int64      `json:"pool_id"`
	DepartmentID     int64      `json:"department_id"`
	Employee         string     `json:"employee"`
	Mode             string     `json:"mode"`
	Status           string     `json:"status"`
	IssuedEpoch      int64      `json:"issued_epoch"`
	OfflineExpiresAt *time.Time `json:"offline_expires_at"`
	CredentialNonce  *string    `json:"-"` // 不外泄,仅用于校验归还凭证
	CreatedAt        time.Time  `json:"created_at"`
	ClosedAt         *time.Time `json:"closed_at"`
}

type BorrowInput struct {
	PoolID           int64
	DepartmentID     int64
	Employee         string
	Mode             string
	IdempotencyKey   string
	OfflineExpiresAt *time.Time
	CredentialNonce  *string
	Now              time.Time
}

type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

func isDuplicateKey(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}

// ---- 租约:签发权威的判据,全部在 MySQL 内 ----

type Lease struct {
	Epoch          int64     `json:"epoch"`
	HolderID       string    `json:"holder_id"`
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
	TakeoverReason string    `json:"takeover_reason"`
	Valid          bool      `json:"valid"` // lease_expires_at > now
}

type TakeoverRecord struct {
	Epoch    int64     `json:"epoch"`
	HolderID string    `json:"holder_id"`
	Reason   string    `json:"reason"`
	At       time.Time `json:"at"`
}

func (s *Store) LeaseState(ctx context.Context, now time.Time) (*Lease, error) {
	var l Lease
	err := s.db.QueryRowContext(ctx,
		`SELECT epoch, holder_id, lease_expires_at, takeover_reason FROM service_lease WHERE id = 1`).
		Scan(&l.Epoch, &l.HolderID, &l.LeaseExpiresAt, &l.TakeoverReason)
	if err != nil {
		return nil, err
	}
	l.Valid = l.LeaseExpiresAt.After(now)
	return &l, nil
}

// AcquireLease 尝试接管租约。单条原子 UPDATE:并发争抢时行锁序列化,
// 后到事务重读最新已提交状态,发现租约已未过期则 WHERE 不命中 ——
// 保证任意时刻只有一个获胜者,epoch 恰好递增一次。
func (s *Store) AcquireLease(ctx context.Context, instanceID, reason string, leaseDur time.Duration, now time.Time) (*Lease, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`UPDATE service_lease
		 SET epoch = epoch + 1, holder_id = ?, lease_expires_at = ?, takeover_reason = ?
		 WHERE id = 1 AND lease_expires_at < ?`,
		instanceID, now.Add(leaseDur), reason, now)
	if err != nil {
		return nil, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, false, err
	}
	if n == 0 {
		tx.Rollback()
		l, gerr := s.LeaseState(ctx, now)
		return l, false, gerr
	}

	var epoch int64
	if err = tx.QueryRowContext(ctx, `SELECT epoch FROM service_lease WHERE id = 1`).Scan(&epoch); err != nil {
		return nil, false, err
	}
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO takeover_history (epoch, holder_id, reason) VALUES (?,?,?)`,
		epoch, instanceID, reason); err != nil {
		return nil, false, err
	}
	if err = tx.Commit(); err != nil {
		return nil, false, err
	}
	l, err := s.LeaseState(ctx, now)
	return l, true, err
}

// RenewLease 持有者续租。holder+epoch 双重匹配:若已被接管则 0 行,调用方必须下台。
func (s *Store) RenewLease(ctx context.Context, instanceID string, epoch int64, leaseDur time.Duration, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE service_lease SET lease_expires_at = ? WHERE id = 1 AND holder_id = ? AND epoch = ?`,
		now.Add(leaseDur), instanceID, epoch)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *Store) TakeoverHistory(ctx context.Context, limit int) ([]TakeoverRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT epoch, holder_id, reason, created_at FROM takeover_history ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TakeoverRecord{}
	for rows.Next() {
		var r TakeoverRecord
		if err := rows.Scan(&r.Epoch, &r.HolderID, &r.Reason, &r.At); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// checkAuthority 围栏检查:签发类写操作必须在事务内确认本实例持有有效租约。
// 与租约更新共用同一行锁,因此"检查-写入"之间不存在权威漂移窗口。
func checkAuthority(tx *sql.Tx, instanceID string, now time.Time) (int64, error) {
	var epoch int64
	var holder string
	var expires time.Time
	err := tx.QueryRow(`SELECT epoch, holder_id, lease_expires_at FROM service_lease WHERE id = 1 FOR UPDATE`).
		Scan(&epoch, &holder, &expires)
	if err != nil {
		return 0, err
	}
	if holder != instanceID || !expires.After(now) {
		return 0, ErrNotAuthoritative
	}
	return epoch, nil
}

// Borrow 借用一个席位(签发,需权威)。
//
// 并发安全模型(全部在 MySQL 事务内,Redis 不参与):
//  1. 幂等键预检:已存在则直接返回原记录(网络重试/跨实例重试场景,只读不围栏)。
//  2. 事务内先锁租约行做围栏检查,确认本实例是当前权威并取回签发代次;
//     再按固定顺序锁 池行 -> 额度行,同一 (池,部门) 的借用被序列化,
//     最后一个席位只会有一个事务看到 count < quota。
//  3. 占用统计使用锁定读,读到最新已提交状态,不受 RR 快照影响。
//  4. 插入撞幂等键唯一约束(与步骤 1 之间的竞态)则回滚重查,返回首次结果。
//
// 返回 (记录, 是否幂等重放, 错误)。
func (s *Store) Borrow(ctx context.Context, instanceID string, in BorrowInput) (*Borrow, bool, error) {
	if b, err := s.borrowByKey(ctx, in.IdempotencyKey); err == nil {
		return b, true, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	epoch, err := checkAuthority(tx, instanceID, in.Now)
	if err != nil {
		return nil, false, err
	}

	var totalSeats int
	err = tx.QueryRowContext(ctx,
		`SELECT total_seats FROM license_pools WHERE id = ? FOR UPDATE`, in.PoolID).Scan(&totalSeats)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, ErrPoolNotFound
	}
	if err != nil {
		return nil, false, err
	}

	var quota int
	err = tx.QueryRowContext(ctx,
		`SELECT quota FROM department_quotas WHERE pool_id = ? AND department_id = ? FOR UPDATE`,
		in.PoolID, in.DepartmentID).Scan(&quota)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, ErrNoQuota
	}
	if err != nil {
		return nil, false, err
	}

	var deptActive int
	if err = tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM borrows WHERE pool_id = ? AND department_id = ? AND status = 'active' FOR UPDATE`,
		in.PoolID, in.DepartmentID).Scan(&deptActive); err != nil {
		return nil, false, err
	}
	if deptActive >= quota {
		return nil, false, ErrDeptQuotaExhausted
	}

	var poolActive int
	if err = tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM borrows WHERE pool_id = ? AND status = 'active' FOR UPDATE`,
		in.PoolID).Scan(&poolActive); err != nil {
		return nil, false, err
	}
	if poolActive >= totalSeats {
		return nil, false, ErrPoolExhausted
	}

	res, err := tx.ExecContext(ctx,
		`INSERT INTO borrows (idempotency_key, pool_id, department_id, employee, mode, issued_epoch, offline_expires_at, credential_nonce)
		 VALUES (?,?,?,?,?,?,?,?)`,
		in.IdempotencyKey, in.PoolID, in.DepartmentID, in.Employee, in.Mode,
		epoch, in.OfflineExpiresAt, in.CredentialNonce)
	if isDuplicateKey(err) {
		// 并发下另一事务先提交了同一幂等键:返回它的结果
		tx.Rollback()
		b, gerr := s.borrowByKey(ctx, in.IdempotencyKey)
		if gerr != nil {
			return nil, false, gerr
		}
		return b, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, false, err
	}
	if err = tx.Commit(); err != nil {
		return nil, false, err
	}
	b, err := s.borrowByID(ctx, id)
	return b, false, err
}

// CloseBorrow 将 active 借用置为终态(returned/reclaimed)。
// 状态迁移只承认 active -> * 一次,天然防止重复归还造成席位重复释放。
// 归还是不围栏的:任何实例都可安全执行(幂等),保证旧凭证交给新实例可用。
func (s *Store) CloseBorrow(ctx context.Context, id int64, toStatus string, now time.Time) (*Borrow, bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE borrows SET status = ?, closed_at = ? WHERE id = ? AND status = 'active'`,
		toStatus, now, id)
	if err != nil {
		return nil, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, false, err
	}
	b, err := s.borrowByID(ctx, id)
	if err != nil {
		return nil, false, err
	}
	return b, n == 1, nil
}

// ReclaimExpired 回收所有已到期的离线席位,返回回收数量。
// 只按到期时间判定,与代次无关:合法签出的离线借用按原有效期计占用。
func (s *Store) ReclaimExpired(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE borrows SET status = 'reclaimed', closed_at = ?
		 WHERE status = 'active' AND mode = 'offline'
		   AND offline_expires_at IS NOT NULL AND offline_expires_at < ?`,
		now, now)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// MigrateDepartment 部门迁移:只改归属,单条 UPDATE 原子完成,
// 席位全程保持占用 —— 不释放、不新增,池级占用不变。
// 目标部门即使超额也允许(与缩减额度同语义:超额只限制新申请)。
func (s *Store) MigrateDepartment(ctx context.Context, instanceID string, borrowID, newDeptID int64, now time.Time) (*Borrow, int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()

	if _, err := checkAuthority(tx, instanceID, now); err != nil {
		return nil, 0, err
	}

	var deptName string
	err = tx.QueryRowContext(ctx, `SELECT name FROM departments WHERE id = ?`, newDeptID).Scan(&deptName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, ErrDepartmentNotFound
	}
	if err != nil {
		return nil, 0, err
	}

	var status string
	var poolID int64
	err = tx.QueryRowContext(ctx,
		`SELECT status, pool_id FROM borrows WHERE id = ? FOR UPDATE`, borrowID).Scan(&status, &poolID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, ErrBorrowNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	if status != StatusActive {
		return nil, 0, ErrBorrowNotActive
	}

	if _, err = tx.ExecContext(ctx,
		`UPDATE borrows SET department_id = ? WHERE id = ? AND status = 'active'`,
		newDeptID, borrowID); err != nil {
		return nil, 0, err
	}

	var targetActive int
	if err = tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM borrows WHERE pool_id = ? AND department_id = ? AND status = 'active' FOR UPDATE`,
		poolID, newDeptID).Scan(&targetActive); err != nil {
		return nil, 0, err
	}
	if err = tx.Commit(); err != nil {
		return nil, 0, err
	}
	b, err := s.borrowByID(ctx, borrowID)
	return b, targetActive, err
}

// AdjustQuota 调整部门额度(upsert)。允许调到当前占用之下:
// 现存借用一律保留,只在 Borrow 的 count >= quota 判断处限制新申请。
func (s *Store) AdjustQuota(ctx context.Context, poolID, deptID int64, quota int, adjustedBy string) (active int, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	if _, err = tx.ExecContext(ctx,
		`INSERT INTO department_quotas (pool_id, department_id, quota, updated_by)
		 VALUES (?,?,?,?)
		 ON DUPLICATE KEY UPDATE quota = VALUES(quota), updated_by = VALUES(updated_by)`,
		poolID, deptID, quota, adjustedBy); err != nil {
		return 0, err
	}
	if err = tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM borrows WHERE pool_id = ? AND department_id = ? AND status = 'active' FOR UPDATE`,
		poolID, deptID).Scan(&active); err != nil {
		return 0, err
	}
	return active, tx.Commit()
}

func (s *Store) borrowByKey(ctx context.Context, key string) (*Borrow, error) {
	return s.scanBorrow(s.db.QueryRowContext(ctx,
		`SELECT id, idempotency_key, pool_id, department_id, employee, mode, status,
		        issued_epoch, offline_expires_at, credential_nonce, created_at, closed_at
		 FROM borrows WHERE idempotency_key = ?`, key))
}

func (s *Store) borrowByID(ctx context.Context, id int64) (*Borrow, error) {
	b, err := s.scanBorrow(s.db.QueryRowContext(ctx,
		`SELECT id, idempotency_key, pool_id, department_id, employee, mode, status,
		        issued_epoch, offline_expires_at, credential_nonce, created_at, closed_at
		 FROM borrows WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrBorrowNotFound
	}
	return b, err
}

func (s *Store) scanBorrow(row *sql.Row) (*Borrow, error) {
	var b Borrow
	err := row.Scan(&b.ID, &b.IdempotencyKey, &b.PoolID, &b.DepartmentID, &b.Employee,
		&b.Mode, &b.Status, &b.IssuedEpoch, &b.OfflineExpiresAt, &b.CredentialNonce, &b.CreatedAt, &b.ClosedAt)
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// ---- 总览查询(结果由 Redis 缓存,见 cache.go)----

type Department struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type DeptUsage struct {
	DepartmentID int64  `json:"department_id"`
	Name         string `json:"name"`
	Quota        int    `json:"quota"`
	Active       int    `json:"active"`
	Online       int    `json:"online"`
	Offline      int    `json:"offline"`
}

type PoolOverview struct {
	ID          int64       `json:"id"`
	Product     string      `json:"product"`
	TotalSeats  int         `json:"total_seats"`
	Active      int         `json:"active"`
	Online      int         `json:"online"`
	Offline     int         `json:"offline"`
	Departments []DeptUsage `json:"departments"`
}

type ActiveBorrow struct {
	ID          int64      `json:"id"`
	PoolID      int64      `json:"pool_id"`
	Product     string     `json:"product"`
	Department  string     `json:"department"`
	DeptID      int64      `json:"department_id"`
	Employee    string     `json:"employee"`
	Mode        string     `json:"mode"`
	IssuedEpoch int64      `json:"issued_epoch"`
	ExpiresAt   *time.Time `json:"expires_at"`
	SecondsLeft *int64     `json:"seconds_left"`
	BorrowedAt  time.Time  `json:"borrowed_at"`
}

type Overview struct {
	Pools         []PoolOverview `json:"pools"`
	ActiveBorrows []ActiveBorrow `json:"active_borrows"`
	Departments   []Department   `json:"departments"`
	GeneratedAt   time.Time      `json:"generated_at"`
}

func (s *Store) Overview(ctx context.Context, now time.Time) (*Overview, error) {
	ov := &Overview{GeneratedAt: now}

	rows, err := s.db.QueryContext(ctx, `SELECT id, name FROM departments ORDER BY id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var d Department
		if err := rows.Scan(&d.ID, &d.Name); err != nil {
			rows.Close()
			return nil, err
		}
		ov.Departments = append(ov.Departments, d)
	}
	rows.Close()

	prows, err := s.db.QueryContext(ctx, `SELECT id, product, total_seats FROM license_pools ORDER BY id`)
	if err != nil {
		return nil, err
	}
	poolIdx := map[int64]*PoolOverview{}
	for prows.Next() {
		var p PoolOverview
		if err := prows.Scan(&p.ID, &p.Product, &p.TotalSeats); err != nil {
			prows.Close()
			return nil, err
		}
		p.Departments = []DeptUsage{}
		ov.Pools = append(ov.Pools, p)
	}
	prows.Close()
	for i := range ov.Pools {
		poolIdx[ov.Pools[i].ID] = &ov.Pools[i]
	}

	urows, err := s.db.QueryContext(ctx,
		`SELECT q.pool_id, q.department_id, d.name, q.quota,
		        COALESCE(SUM(b.status = 'active'), 0),
		        COALESCE(SUM(b.status = 'active' AND b.mode = 'online'), 0),
		        COALESCE(SUM(b.status = 'active' AND b.mode = 'offline'), 0)
		 FROM department_quotas q
		 JOIN departments d ON d.id = q.department_id
		 LEFT JOIN borrows b ON b.pool_id = q.pool_id AND b.department_id = q.department_id
		 GROUP BY q.pool_id, q.department_id, d.name, q.quota
		 ORDER BY q.pool_id, q.department_id`)
	if err != nil {
		return nil, err
	}
	for urows.Next() {
		var u DeptUsage
		var poolID int64
		if err := urows.Scan(&poolID, &u.DepartmentID, &u.Name, &u.Quota, &u.Active, &u.Online, &u.Offline); err != nil {
			urows.Close()
			return nil, err
		}
		p, ok := poolIdx[poolID]
		if !ok {
			continue
		}
		p.Departments = append(p.Departments, u)
	}
	urows.Close()

	// 池级占用独立统计:迁移到无额度部门的席位也必须计入
	aprows, err := s.db.QueryContext(ctx,
		`SELECT pool_id, COUNT(*),
		        COALESCE(SUM(mode = 'online'), 0),
		        COALESCE(SUM(mode = 'offline'), 0)
		 FROM borrows WHERE status = 'active' GROUP BY pool_id`)
	if err != nil {
		return nil, err
	}
	for aprows.Next() {
		var poolID int64
		var active, online, offline int
		if err := aprows.Scan(&poolID, &active, &online, &offline); err != nil {
			aprows.Close()
			return nil, err
		}
		if p, ok := poolIdx[poolID]; ok {
			p.Active, p.Online, p.Offline = active, online, offline
		}
	}
	aprows.Close()

	brows, err := s.db.QueryContext(ctx,
		`SELECT b.id, b.pool_id, p.product, d.name, d.id, b.employee, b.mode, b.issued_epoch, b.offline_expires_at, b.created_at
		 FROM borrows b
		 JOIN license_pools p ON p.id = b.pool_id
		 JOIN departments d ON d.id = b.department_id
		 WHERE b.status = 'active'
		 ORDER BY b.id DESC`)
	if err != nil {
		return nil, err
	}
	ov.ActiveBorrows = []ActiveBorrow{}
	for brows.Next() {
		var ab ActiveBorrow
		if err := brows.Scan(&ab.ID, &ab.PoolID, &ab.Product, &ab.Department, &ab.DeptID, &ab.Employee,
			&ab.Mode, &ab.IssuedEpoch, &ab.ExpiresAt, &ab.BorrowedAt); err != nil {
			brows.Close()
			return nil, err
		}
		if ab.ExpiresAt != nil {
			left := ab.ExpiresAt.Unix() - now.Unix()
			ab.SecondsLeft = &left
		}
		ov.ActiveBorrows = append(ov.ActiveBorrows, ab)
	}
	brows.Close()
	return ov, nil
}

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }
