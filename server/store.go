package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
}

type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

func isDuplicateKey(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}

// Borrow 借用一个席位。
//
// 并发安全模型(全部在 MySQL 事务内,Redis 不参与):
//  1. 幂等键预检:已存在则直接返回原记录(网络重试场景)。
//  2. 事务内按固定顺序锁定 池行 -> 额度行(FOR UPDATE),同一 (池,部门)
//     的借用被序列化,最后一个席位只会有一个事务看到 count < quota。
//  3. 占用数统计使用锁定读,读到的是最新已提交状态,不受 RR 快照影响。
//  4. 插入时若撞上幂等键唯一约束(与步骤 1 之间存在竞态),回滚后重查
//     原记录返回,调用方拿到的仍是"第一次借用的结果"。
//
// 返回 (记录, 是否幂等重放, 错误)。
func (s *Store) Borrow(ctx context.Context, in BorrowInput) (*Borrow, bool, error) {
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
		`INSERT INTO borrows (idempotency_key, pool_id, department_id, employee, mode, offline_expires_at, credential_nonce)
		 VALUES (?,?,?,?,?,?,?)`,
		in.IdempotencyKey, in.PoolID, in.DepartmentID, in.Employee, in.Mode,
		in.OfflineExpiresAt, in.CredentialNonce)
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
// 返回 (最新记录, 本次是否真正发生了状态迁移, 错误)。
// 状态迁移只承认 active -> * 一次,天然防止重复归还造成席位重复释放。
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
		        offline_expires_at, credential_nonce, created_at, closed_at
		 FROM borrows WHERE idempotency_key = ?`, key))
}

func (s *Store) borrowByID(ctx context.Context, id int64) (*Borrow, error) {
	b, err := s.scanBorrow(s.db.QueryRowContext(ctx,
		`SELECT id, idempotency_key, pool_id, department_id, employee, mode, status,
		        offline_expires_at, credential_nonce, created_at, closed_at
		 FROM borrows WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrBorrowNotFound
	}
	return b, err
}

func (s *Store) scanBorrow(row *sql.Row) (*Borrow, error) {
	var b Borrow
	err := row.Scan(&b.ID, &b.IdempotencyKey, &b.PoolID, &b.DepartmentID, &b.Employee,
		&b.Mode, &b.Status, &b.OfflineExpiresAt, &b.CredentialNonce, &b.CreatedAt, &b.ClosedAt)
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
	ID           int64      `json:"id"`
	PoolID       int64      `json:"pool_id"`
	Product      string     `json:"product"`
	Department   string     `json:"department"`
	Employee     string     `json:"employee"`
	Mode         string     `json:"mode"`
	ExpiresAt    *time.Time `json:"expires_at"`
	SecondsLeft  *int64     `json:"seconds_left"`
	BorrowedAt   time.Time  `json:"borrowed_at"`
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
		p.Active += u.Active
		p.Online += u.Online
		p.Offline += u.Offline
	}
	urows.Close()

	brows, err := s.db.QueryContext(ctx,
		`SELECT b.id, b.pool_id, p.product, d.name, b.employee, b.mode, b.offline_expires_at, b.created_at
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
		if err := brows.Scan(&ab.ID, &ab.PoolID, &ab.Product, &ab.Department, &ab.Employee,
			&ab.Mode, &ab.ExpiresAt, &ab.BorrowedAt); err != nil {
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

func (s *Store) String() string { return fmt.Sprintf("Store{%p}", s.db) }
