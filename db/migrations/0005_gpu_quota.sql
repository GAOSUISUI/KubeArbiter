-- 0005: GPU 扩展资源配额（P3 AI 任务调度）。
-- quotas.resource 增加 'gpu' 维度；hard 的语义对 gpu 是整卡数（millicore / byte / 卡）。
-- 幂等：重复执行不会报错；既有租户补 hard=0 的 gpu 行（0 = 不限，与缺行语义一致）。
ALTER TABLE quotas DROP CONSTRAINT IF EXISTS quotas_resource_check;
ALTER TABLE quotas ADD CONSTRAINT quotas_resource_check
    CHECK (resource IN ('cpu','memory','gpu'));

INSERT INTO quotas (tenant_id, resource, hard, fair_share)
SELECT t.id, 'gpu', 0, 0
FROM tenants t
WHERE NOT EXISTS (
    SELECT 1 FROM quotas q WHERE q.tenant_id = t.id AND q.resource = 'gpu'
);
