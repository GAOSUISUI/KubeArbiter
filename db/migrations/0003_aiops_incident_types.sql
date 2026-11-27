-- 0003: AIOps 事件类型扩展。
-- 原有 7 类偏应用发布语义，节点/容量/指标类事件需要独立类型，便于筛选与统计。
-- 幂等：重复执行不会报错。
ALTER TABLE incidents DROP CONSTRAINT IF EXISTS incidents_type_check;
ALTER TABLE incidents ADD CONSTRAINT incidents_type_check
    CHECK (type IN ('CRASH_LOOP','IMAGE_PULL_BACKOFF','READINESS_FAIL','OOM_KILLED',
                    'DOWNSTREAM_TIMEOUT','EMPTY_ENDPOINTS','CONFIG_ERROR',
                    'NODE_NOT_READY','NODE_PRESSURE','NODE_HIGH_USAGE','METRICS_STALE',
                    'POD_PENDING_LONG','POD_READINESS_FAIL','QUOTA_NEAR'));
