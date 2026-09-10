-- LicenseHub 浮动许可证管理 schema
-- 席位正确性依赖:事务(FOR UPDATE 行锁序列化借用)+ 唯一约束(幂等键、池-部门额度)

CREATE TABLE IF NOT EXISTS departments (
  id   BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  name VARCHAR(64) NOT NULL UNIQUE
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS license_pools (
  id          BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  product     VARCHAR(64) NOT NULL UNIQUE,
  total_seats INT NOT NULL CHECK (total_seats >= 0)
) ENGINE=InnoDB;

-- 部门额度:每个 (池, 部门) 至多一行,唯一约束保证
CREATE TABLE IF NOT EXISTS department_quotas (
  id            BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  pool_id       BIGINT UNSIGNED NOT NULL,
  department_id BIGINT UNSIGNED NOT NULL,
  quota         INT NOT NULL CHECK (quota >= 0),
  updated_by    VARCHAR(64) NOT NULL DEFAULT '',
  updated_at    DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uq_pool_dept (pool_id, department_id),
  CONSTRAINT fk_quota_pool FOREIGN KEY (pool_id) REFERENCES license_pools (id),
  CONSTRAINT fk_quota_dept FOREIGN KEY (department_id) REFERENCES departments (id)
) ENGINE=InnoDB;

-- 借用记录:幂等键唯一约束是"网络重试返回原借用结果"的底层保证
-- issued_epoch 记录签发时的权威代次,离线凭证随其携带
CREATE TABLE IF NOT EXISTS borrows (
  id                 BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  idempotency_key    VARCHAR(80) NOT NULL,
  pool_id            BIGINT UNSIGNED NOT NULL,
  department_id      BIGINT UNSIGNED NOT NULL,
  employee           VARCHAR(64) NOT NULL,
  mode               ENUM('online','offline') NOT NULL,
  status             ENUM('active','returned','reclaimed') NOT NULL DEFAULT 'active',
  issued_epoch       BIGINT NOT NULL DEFAULT 0,
  offline_expires_at DATETIME(3) NULL,
  credential_nonce   VARCHAR(64) NULL,
  created_at         DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  closed_at          DATETIME(3) NULL,
  UNIQUE KEY uq_idempotency_key (idempotency_key),
  KEY idx_pool_dept_status (pool_id, department_id, status),
  KEY idx_reap (status, mode, offline_expires_at)
) ENGINE=InnoDB;

-- 服务租约(单行):签发权威的唯一判据。epoch 单调递增,
-- 只有在租约过期时才能被接管;签发事务在同一行上做围栏检查。
-- 权威的判定完全在 MySQL 内,Redis 不参与。
CREATE TABLE IF NOT EXISTS service_lease (
  id               TINYINT UNSIGNED PRIMARY KEY CHECK (id = 1),
  epoch            BIGINT NOT NULL,
  holder_id        VARCHAR(64) NOT NULL,
  lease_expires_at DATETIME(3) NOT NULL,
  takeover_reason  VARCHAR(128) NOT NULL DEFAULT '',
  updated_at       DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3)
) ENGINE=InnoDB;

INSERT INTO service_lease (id, epoch, holder_id, lease_expires_at, takeover_reason)
VALUES (1, 0, '', '1970-01-01 00:00:00.000', 'bootstrap')
ON DUPLICATE KEY UPDATE id = id;

-- 接管历史:管理员页面展示接管原因
CREATE TABLE IF NOT EXISTS takeover_history (
  id         BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  epoch      BIGINT NOT NULL,
  holder_id  VARCHAR(64) NOT NULL,
  reason     VARCHAR(128) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
) ENGINE=InnoDB;
