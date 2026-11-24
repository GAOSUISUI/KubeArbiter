-- 0002: 策略增加公平模式，支持 DRF 节点级试算与 WEIGHTED_SHARE 基线 A/B 对比。
-- 幂等：重复执行不会报错。
ALTER TABLE policies
    ADD COLUMN IF NOT EXISTS fairness_mode varchar(16) NOT NULL DEFAULT 'DRF';

ALTER TABLE policies DROP CONSTRAINT IF EXISTS policies_fairness_mode_check;
ALTER TABLE policies ADD CONSTRAINT policies_fairness_mode_check
    CHECK (fairness_mode IN ('DRF','WEIGHTED_SHARE'));

COMMENT ON COLUMN policies.fairness_mode IS 's_fair 计算方式：DRF 节点级 DRF 试算 / WEIGHTED_SHARE 全局份额基线';
