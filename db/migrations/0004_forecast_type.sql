-- 0004: 预测资源耗尽事件类型。
-- P2 预测与容量规划：当节点按当前负载趋势预计在预警窗口内达到容量上限时，
-- 检测器产生 NODE_FORECAST_EXHAUST（WARN，建议性质，不触发任何自动执行）。
-- 幂等：重复执行不会报错。
ALTER TABLE incidents DROP CONSTRAINT IF EXISTS incidents_type_check;
ALTER TABLE incidents ADD CONSTRAINT incidents_type_check
    CHECK (type IN ('CRASH_LOOP','IMAGE_PULL_BACKOFF','READINESS_FAIL','OOM_KILLED',
                    'DOWNSTREAM_TIMEOUT','EMPTY_ENDPOINTS','CONFIG_ERROR',
                    'NODE_NOT_READY','NODE_PRESSURE','NODE_HIGH_USAGE','METRICS_STALE',
                    'POD_PENDING_LONG','POD_READINESS_FAIL','QUOTA_NEAR',
                    'NODE_FORECAST_EXHAUST'));
