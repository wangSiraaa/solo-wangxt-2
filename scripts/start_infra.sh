#!/usr/bin/env bash
# 启动本地基础设施(MariaDB + Redis),幂等;然后初始化数据库
set -e
ROOT=/workspace/.tools
R=$ROOT/rootfs
export LD_LIBRARY_PATH=$R/usr/lib/aarch64-linux-gnu:$R/lib/aarch64-linux-gnu
mkdir -p $ROOT/run

if ! $R/usr/bin/mysqladmin --socket=$ROOT/run/mysql.sock -u root ping >/dev/null 2>&1; then
  echo "启动 MariaDB…"
  [ -d $ROOT/mysql-data/mysql ] || \
    $R/usr/bin/mariadb-install-db --no-defaults --basedir=$R/usr \
      --datadir=$ROOT/mysql-data --auth-root-authentication-method=normal >/dev/null
  $R/usr/sbin/mariadbd --no-defaults --basedir=$R/usr --datadir=$ROOT/mysql-data \
    --socket=$ROOT/run/mysql.sock --port=3306 --bind-address=127.0.0.1 \
    --pid-file=$ROOT/run/mariadb.pid --lc-messages-dir=$R/usr/share/mysql \
    >$ROOT/run/mariadb.log 2>&1 &
  for i in $(seq 1 30); do
    $R/usr/bin/mysqladmin --socket=$ROOT/run/mysql.sock -u root ping >/dev/null 2>&1 && break
    sleep 1
  done
fi

if ! $R/usr/bin/redis-cli -p 6379 ping >/dev/null 2>&1; then
  echo "启动 Redis…"
  $R/usr/bin/redis-server --port 6379 --bind 127.0.0.1 --dir $ROOT/run \
    --daemonize yes --logfile $ROOT/run/redis.log
fi

# 数据库、账号、schema、种子数据(全部幂等)
$R/usr/bin/mysql --socket=$ROOT/run/mysql.sock -u root <<'SQL'
CREATE DATABASE IF NOT EXISTS licensehub CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
CREATE USER IF NOT EXISTS 'app'@'%' IDENTIFIED BY 'app123';
GRANT ALL PRIVILEGES ON `licensehub%`.* TO 'app'@'%';
DROP USER IF EXISTS ''@'localhost';
FLUSH PRIVILEGES;
SQL
$R/usr/bin/mysql -h 127.0.0.1 -u app -papp123 licensehub < /workspace/server/schema.sql
$R/usr/bin/mysql -h 127.0.0.1 -u app -papp123 licensehub < /workspace/server/seed.sql
echo "基础设施就绪: MySQL 127.0.0.1:3306 / Redis 127.0.0.1:6379"
