-- 演示数据:三个部门、三个许可证池、部门额度
INSERT INTO departments (name) VALUES
  ('结构设计部'), ('仿真分析部'), ('嵌入式软件部')
  ON DUPLICATE KEY UPDATE name = name;

INSERT INTO license_pools (product, total_seats) VALUES
  ('AutoCAD', 10), ('ANSYS', 4), ('MATLAB', 6)
  ON DUPLICATE KEY UPDATE total_seats = VALUES(total_seats);

INSERT INTO department_quotas (pool_id, department_id, quota, updated_by)
  SELECT p.id, d.id, v.quota, 'seed'
  FROM (SELECT 1 AS pool_id, 1 AS dept_id, 5 AS quota
        UNION ALL SELECT 1, 2, 3
        UNION ALL SELECT 2, 2, 3
        UNION ALL SELECT 2, 1, 1
        UNION ALL SELECT 3, 3, 4
        UNION ALL SELECT 3, 2, 2) v
  JOIN license_pools p ON p.id = v.pool_id
  JOIN departments d ON d.id = v.dept_id
  ON DUPLICATE KEY UPDATE quota = VALUES(quota);
