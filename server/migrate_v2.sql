-- v1 -> v2 迁移(MariaDB 幂等):高可用租约与签发代次
ALTER TABLE borrows ADD COLUMN IF NOT EXISTS issued_epoch BIGINT NOT NULL DEFAULT 0;

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

CREATE TABLE IF NOT EXISTS takeover_history (
  id         BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  epoch      BIGINT NOT NULL,
  holder_id  VARCHAR(64) NOT NULL,
  reason     VARCHAR(128) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
) ENGINE=InnoDB;
