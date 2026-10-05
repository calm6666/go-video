-- 000004_seed_op_permission.sql
-- 目的：把 gateway/admin 运营面用到的权限点一次性登记进 op_permission。
--
-- 为什么需要这条迁移：`AdminPermission` 中间件按「登录管理员的角色 → 权限点」判定，
-- 判定不中一律 403（fail-closed）。前几轮只在中间件里登记了路由↔权限点映射，
-- 库里却没有任何权限点行，结果是**没有任何角色能被授予任何运营入口**——
-- 后台能登录、能读，但所有写接口对所有人 403。本迁移补上这一段。
--
-- 数据来源（单一事实源）：
--   gateway/admin/internal/middleware/adminpermissionmiddleware.go 的 routePermissions 表。
--   本文件的 (resource, action) 集合必须与该表去重后的集合**逐字相等**，两侧都多出或缺少都算漂移，
--   由 `gateway/admin/internal/middleware/seed_permission_test.go` 双向断言把守（失败信息会点名
--   缺/多的那一组）。
--   改法：先改中间件表，再在本文件对应域补/删一行；不要反过来靠删权限点让测试变绿。
--   当前 routePermissions 有 166 条路由项、去重后 162 组权限点（四对路由各共用一个权限点：
--   operation 的 task/get 与 task/list、openplatform 的 application/get 与 application/list、
--   account 的 cache/del 与 cache/clear、member 的 morals/update 与 moral/update）。
--   读接口刻意不进 routePermissions（除 operation 域早期已登记的几条，以及**被读主体明确**的
--   定向个人数据读：spm 的兴趣画像、feature-store 的主体特征、member 的实名正查/证件号反查/
--   登录日志），另有两条「形状是 GET、后果是写」的入口（account 缓存失效、inbox 未读重算）
--   也一并挂判定——读写形状不该决定鉴权形状。这些例外由
--   gateway/admin/internal/middleware/route_permission_drift_test.go 的 protectedGetAllowlist
--   逐条点名把守。种子因此只覆盖中间件真正要求的权限点，不多发。
--
-- owner：运营平台（operation 服务，op_* 前缀，deploy/migrations/operation）；
--   影响范围：只 INSERT op_permission 的权限点行，不建表、不改表结构。
--   本文件不动 op_role / op_role_permission / op_admin_role：
--   「谁该拿到哪个权限点」是运营侧的组织决策，不由迁移代替人做（AGENTS.md §5）。
--   2026-09-22 更新：角色与绑定改由 000005_seed_op_role_grants.sql 与
--   000006_seed_op_role_grants_stage12.sql 以**派生**方式建立
--   （域名与成员都从本文件的 domain/action 列现算，不维护手工对照表），本条判断仍然成立——
--   它们只建「有哪些角色、角色拿到什么」，依然不写 op_admin_role，即不代替人决定谁拿角色。
--   000006 存在的原因就是本文件后来新增了 13 个阶段 1-2 域的权限点：
--   角色绑定是 INSERT...SELECT，晚到的权限点不会被既有角色自动纳入，必须再派生一轮。
--
-- 幂等口径：全部 INSERT IGNORE，靠 op_permission.uniq_resource_action (resource, action)
--   去重，可重复执行；已存在的行不覆盖描述文本（description 只作展示，不作为判定依据）。
--   ctime 固定 0：表示「由迁移登记，不是人工 CreatePermission 建的」，与页面统计口径无关。
--
-- domain 取 resource 冒号前缀（与 000001 的列注释一致），ListPermissions 的
--   idx_domain_resource_action 因此能按域分组返回。
--
-- 广告相关权限点刻意不生成（AGENTS.md §1：广告投放/广告位分析/广告推荐仍范围外）。
--   商业化里的真实资金能力（退款到卡、提现、打款出金、对账、发票）也没有权限点：
--   这些动作服务侧就返回 FailedPrecondition，不给后台开口子。
--
-- 锁风险：只 INSERT 新行，不改表结构、不 UPDATE 既有行，无在线锁风险。
-- 回滚（删权限点即让对应后台入口对所有角色立刻 403，先确认没有在用的角色绑定再执行）：
--   DELETE FROM `op_permission` WHERE `domain` IN
--     ('operation','ops','audit','cron','live','recommend','collector','pm','spm','feature',
--      'membership','payment','order','coin','revenue','openplatform',
--      'account','member','video','catalog','rights','moderation','transcode','danmaku',
--      'search','risk','comment','notify','inbox');
--   注：只删权限点、不删 op_role_permission 绑定；残留的绑定指向不存在的权限点，
--   等价于「该角色什么都没有」，重新跑本迁移即可恢复。

-- ==== 种子数据 ====
-- 以下 162 条与 routePermissions 去重后逐一对应；新增/删除权限点先改中间件表，再在此补/删同一组
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('operation:admin_user', 'create', 'operation', '后台管理员账号：新建', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('operation:admin_user', 'update', 'operation', '后台管理员账号：修改', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('operation:admin_user', 'disable', 'operation', '后台管理员账号：停用', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('operation:admin_user', 'read', 'operation', '后台管理员账号：读取', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('operation:role', 'assign', 'operation', '后台角色：分配', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('operation:role', 'create', 'operation', '后台角色：新建', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('operation:role', 'read', 'operation', '后台角色：读取', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('operation:role', 'delete', 'operation', '后台角色：删除', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('operation:permission', 'read', 'operation', '后台权限点：读取', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('operation:permission', 'create', 'operation', '后台权限点：新建', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('operation:menu', 'read', 'operation', '后台菜单：读取', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('operation:menu', 'update', 'operation', '后台菜单：修改', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('ops:config', 'read', 'ops', '运营配置项：读取', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('ops:config', 'update', 'ops', '运营配置项：修改', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('operation:task', 'create', 'operation', '异步运营任务：新建', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('operation:task', 'read', 'operation', '异步运营任务：读取', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('operation:task', 'cancel', 'operation', '异步运营任务：取消', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('operation:task', 'run', 'operation', '异步运营任务：执行', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('operation:audit', 'read', 'operation', '后台操作审计：读取', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('audit:export', 'create', 'audit', '审计导出：新建', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('audit:export', 'run', 'audit', '审计导出：执行', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('audit:retention', 'update', 'audit', '审计留存策略：修改（改留存期影响审计证据可得性）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('audit:archive', 'create', 'audit', '审计归档：新建', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('ops:config', 'publish', 'ops', '运营配置项：发布生效（生效到线上读路径，与草稿修改分权）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('ops:config', 'rollback', 'ops', '运营配置项：回滚', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('ops:rollout', 'update', 'ops', '灰度放量规则：修改', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('ops:rollout', 'enable', 'ops', '灰度放量规则：启用', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('ops:topic', 'update', 'ops', '运营专题：修改', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('ops:topic_item', 'update', 'ops', '专题条目：修改', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('ops:slot', 'update', 'ops', '运营资源位：修改', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('ops:slot_item', 'update', 'ops', '资源位条目：修改', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('ops:switch', 'update', 'ops', '功能开关：修改', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('ops:cache', 'refresh', 'ops', '运营缓存：刷新', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('cron:task', 'create', 'cron', '定时任务登记：新建', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('cron:task', 'update', 'cron', '定时任务登记：修改', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('cron:task', 'pause', 'cron', '定时任务登记：暂停', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('cron:task', 'resume', 'cron', '定时任务登记：恢复', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('cron:task', 'disable', 'cron', '定时任务登记：停用', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('cron:task', 'trigger', 'cron', '定时任务登记：手工触发', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('cron:run', 'retry', 'cron', '任务执行记录：重试', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('cron:checkpoint', 'update', 'cron', '任务断点：修改', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('live:room', 'close', 'live', '直播间：关闭', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('live:ban', 'create', 'live', '直播封禁：新建', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('live:ban', 'lift', 'live', '直播封禁：解除', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('live:setting', 'update', 'live', '直播设置：修改', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('live:area', 'update', 'live', '直播分区：修改', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('live:stream', 'close', 'live', '直播流：关闭', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('live:key', 'revoke', 'live', '推流密钥：吊销（主播需重新获取）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('live:node', 'update', 'live', '直播接入节点：修改', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('live:event', 'retry', 'live', '直播事件投递：重试', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('live:connection', 'kick', 'live', '直播长连接：踢下线', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('live:route', 'drain', 'live', '直播路由：摘流', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('live:broadcast', 'send', 'live', '直播广播：下发', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('live:quota', 'update', 'live', '直播配额：修改', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('recommend:pool', 'update', 'recommend', '召回候选池：修改', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('recommend:pool', 'publish', 'recommend', '召回候选池：发布生效（切换后立即影响全站召回结果）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('recommend:pool', 'rollback', 'recommend', '召回候选池：回滚', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('recommend:pool', 'prune', 'recommend', '召回候选池：清理历史版本', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('recommend:model', 'create', 'recommend', '排序模型：新建', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('recommend:model', 'state', 'recommend', '排序模型：切换状态', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('recommend:feature', 'create', 'recommend', '排序特征配置：新建', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('recommend:experiment', 'create', 'recommend', '推荐实验：新建', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('recommend:experiment', 'state', 'recommend', '推荐实验：切换状态', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('live:transcode', 'start', 'live', '直播转码任务：启动', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('live:transcode', 'stop', 'live', '直播转码任务：停止', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('live:transcode', 'retry', 'live', '直播转码任务：重试', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('live:transcode', 'cancel', 'live', '直播转码任务：取消', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('live:output', 'update', 'live', '直播输出模板：修改', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('live:output', 'offline', 'live', '直播输出模板：下线', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('live:record', 'start', 'live', '直播录制：启动', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('live:record', 'stop', 'live', '直播录制：停止', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('live:replay', 'submit', 'live', '直播回放：提交', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('live:replay', 'bind', 'live', '直播回放：绑定', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('live:replay', 'state', 'live', '直播回放：切换状态', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('live:retention', 'submit', 'live', '直播录制留存：提交', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('collector:delivery', 'retry', 'collector', '行为事件投递：重试', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('collector:deadletter', 'replay', 'collector', '行为事件死信：重放', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('collector:policy', 'update', 'collector', '采集与采样策略：修改', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('collector:policy', 'enable', 'collector', '采集与采样策略：启用', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('pm:report', 'handle', 'pm', '私信举报：处置', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('pm:retention', 'purge', 'pm', '私信留存清除：清除', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('membership:plan', 'update', 'membership', '会员套餐：修改（只改 DRAFT 草稿，已上架档改价须新建草稿再切换）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('membership:plan', 'publish', 'membership', '会员套餐：发布生效', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('membership:grant', 'create', 'membership', '会员身份授予：新建（直接开时长并留台账；沙箱台账无真实扣款）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('membership:grant', 'revoke', 'membership', '会员身份授予：收回（退款回收与运营纠错共用此位）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('membership:entitlement', 'update', 'membership', '权益码目录：修改', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('payment:recharge', 'settle', 'payment', '沙箱充值单：受理（无渠道回调验签，只落沙箱台账）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('payment:balance', 'adjust', 'payment', '余额台账：调整（只写余额调整流水，不到卡、不出金）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('order:refund', 'approve', 'order', '订单退款审批：批准（退到沙箱余额并回收权益，不到银行卡）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('order:refund', 'reject', 'order', '订单退款审批：驳回', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('coin:grant', 'create', 'coin', '硬币发放：新建', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('revenue:rule', 'update', 'revenue', '创作者分成规则：修改', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('revenue:rule', 'publish', 'revenue', '创作者分成规则：发布生效', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('revenue:enrollment', 'update', 'revenue', '创作者分成参与：修改', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('revenue:settlement', 'create', 'revenue', '创作者结算单：新建', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('revenue:settlement', 'confirm', 'revenue', '创作者结算单：确认（只确认应计金额，不代表打款）', 0);

INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('spm:interest', 'read', 'spm', '用户兴趣画像：定向读取（唯一挂判定的 spm 读口，能看聚合热度不等于能逐个翻画像）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('spm:definition', 'create', 'spm', '指标口径：登记新版本（只能新增，已登记版本不可原地改）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('spm:definition', 'state', 'spm', '指标口径：上下架（DRAFT/ACTIVE/RETIRED 迁移，激活即改变后续所有窗口的解释）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('spm:job', 'create', 'spm', '指标聚合作业：提交（排队回填/重算，不改口径定义）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('spm:metric', 'recompute', 'spm', '指标历史窗口：按指定口径版本重算（唯一正当的「让已发布的数变掉」路径）', 0);

-- ==== feature-store 域（阶段 4 行为分析：特征定义/版本/回填/隐私） ====
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('feature:entity-value', 'read', 'feature', '主体特征导出：定向读取（唯一挂判定的特征读口，能看目录不等于能逐个翻个人值）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('feature:definition', 'create', 'feature', '特征定义：登记新版本（强制 DRAFT 入库，注册即生效等于绕过评审）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('feature:definition', 'state', 'feature', '特征定义：状态迁移（DRAFT/ACTIVE/RETIRED，RETIRED 让在线读侧改走默认值）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('feature:definition', 'privacy', 'feature', '特征定义：调整隐私级别（只改谁能读、不改值语义，因此单独留痕）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('feature:active-version', 'switch', 'feature', '特征 ACTIVE 版本指针：切换（切换后所有在线读换一版，与单版本上下架后果不同）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('feature:backfill-job', 'create', 'feature', '特征回填作业：提交（补写历史值会覆盖既有值，属写数据不属改定义）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('feature:entity-value', 'erase', 'feature', '主体特征：按隐私工单擦除（不可逆；服务侧操作人前缀白名单二次把关）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('feature:value', 'purge', 'feature', '特征值：手动补跑一轮 TTL 过期清理（常态由 cron 调用，不可逆）', 0);

-- ==== open-platform 域（第三方开放平台：应用/密钥/scope/授权/配额/回调投递） ====
-- 读面只有 application 一项挂判定，且 quota-policy / quota / webhook / delivery 四个台账读也挂：
-- 本域的读对象不是「某个内容的热度」而是「谁在调用我们、能调多少、推给了谁」，
-- 这些是排查第三方故障的第一手证据，也是权限收紧时最容易漏掉的口子。
-- /scope/list（scope 目录）刻意不在这里——它是纯目录，且契约里没有操作者位。
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('openplatform:application', 'read', 'openplatform', '第三方应用：详情与检索（详情含 app_key/回调配置，能看到目录不等于能逐个翻应用）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('openplatform:application', 'state', 'openplatform', '第三方应用：状态机推进（审核通过/封禁/下线，决定第三方整体能不能调用）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('openplatform:secret', 'rotate', 'openplatform', '应用密钥：轮换（旧密钥按宽限期失效，签出去的请求会开始 401）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('openplatform:secret', 'revoke', 'openplatform', '应用密钥：吊销（怀疑泄露时的紧急止血；secret_id=0 一次吊销全部生效密钥）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('openplatform:scope', 'grant', 'openplatform', '应用 scope：授予/回收（决定第三方能读到哪些用户数据范围）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('openplatform:authorization', 'revoke', 'openplatform', '用户授权：撤销（USER_ALL 会让该用户对所有第三方应用的授权一起失效，blast radius 最大）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('openplatform:quota-policy', 'read', 'openplatform', '配额规则：读取（看清「这个应用被限成多少」是限流排障的前提）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('openplatform:quota-policy', 'upsert', 'openplatform', '配额规则：新增/更新（直接改变第三方能打多快；policy_id=0 为新建）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('openplatform:quota', 'read', 'openplatform', '配额用量：读取窗口投影（判「是否被限流」的证据）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('openplatform:quota', 'recompute', 'openplatform', '配额用量：从调用流水重算投影（修漂移；dry_run 位由调用方显式给）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('openplatform:webhook', 'read', 'openplatform', '回调端点：读取（端点地址是第三方接收面，属敏感配置）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('openplatform:webhook', 'delete', 'openplatform', '回调端点：删除（同时抑制其未投递任务；改地址只能由归属者重新注册）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('openplatform:delivery', 'read', 'openplatform', '回调投递台账：读取（事件到底送没送到，只有这一条能看到）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('openplatform:delivery', 'retry', 'openplatform', '回调投递：死信重放（明知上游未修好也要再打一次，故 reason 必填；不制造新事件）', 0);

-- ==== account 域（阶段 1-2 补齐：账号缓存失效） ====
-- 划线口径与其余域一致：写入口全部挂判定，读面默认免鉴权，只有「被读主体明确的个人数据定向读」例外（见 gateway/admin/README.md）。
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('account:cache', 'invalidate', 'account', '账号缓存：失效指定 mid 的资料/会话缓存（GET 入参与 JSON 消息两个入口共用本点）', 0);

-- ==== member 域（阶段 1-2 补齐：user-profile 节操值/经验值/实名与登录日志） ====
-- 划线口径与其余域一致：写入口全部挂判定，读面默认免鉴权，只有「被读主体明确的个人数据定向读」例外（见 gateway/admin/README.md）。
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('member:moral', 'update', 'member', '节操值：变更（单个与批量两个入口共用本点，都直接改用户可见数值）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('member:moral', 'undo', 'member', '节操值：撤销一次变更（纠偏动作，不隐含「能再改一次」）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('member:exp', 'set', 'member', '经验值：直接覆盖绝对值（能把已有经验清零，比按增量加重要）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('member:exp', 'update', 'member', '经验值：按增量追加', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('member:property-review', 'create', 'member', '用户属性审核：提交一条待审变更（本点不含批准，生效由审核侧决定）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('member:realname', 'read', 'member', '实名信息：按 mid 定向读脱敏结果（被读主体明确的个人数据读面）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('member:realname', 'reverse-lookup', 'member', '实名信息：按证件号反查 mid（可对证件号清单批量配对，泄露面与正查不同，单列一点）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('member:login-log', 'read', 'member', '登录日志：读某个用户的登录明细（被读主体明确的个人数据读面）', 0);

-- ==== video 域（阶段 1-2 补齐：稿件状态机） ====
-- 划线口径与其余域一致：写入口全部挂判定，读面默认免鉴权，只有「被读主体明确的个人数据定向读」例外（见 gateway/admin/README.md）。
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('video:submission', 'transition', 'video', '稿件：推进状态机（服务侧已禁止直接置为 PUBLISHED，仍须走审核链）', 0);

-- ==== catalog 域（阶段 1-2 补齐：版权目录作品/季/集） ====
-- 划线口径与其余域一致：写入口全部挂判定，读面默认免鉴权，只有「被读主体明确的个人数据定向读」例外（见 gateway/admin/README.md）。
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('catalog:work', 'create', 'catalog', '版权目录：新建作品', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('catalog:season', 'create', 'catalog', '版权目录：新建季', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('catalog:episode', 'create', 'catalog', '版权目录：新建集', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('catalog:episode', 'publish', 'catalog', '版权目录：集上架（内容对公众可见的开关，需版权窗口与审核结论）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('catalog:episode', 'offline', 'catalog', '版权目录：集下架（撤回公众可见性，方向与上架相反，故不共用一个点）', 0);

-- ==== rights 域（阶段 1-2 补齐：版权合同与播放窗口） ====
-- 划线口径与其余域一致：写入口全部挂判定，读面默认免鉴权，只有「被读主体明确的个人数据定向读」例外（见 gateway/admin/README.md）。
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('rights:contract', 'create', 'rights', '版权合同：新建（窗口授权的依据）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('rights:window', 'create', 'rights', '播放窗口：新建（决定内容什么时候能播）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('rights:window', 'expire', 'rights', '播放窗口：手动提前过期（应急下线，与新建方向相反，不可逆）', 0);

-- ==== moderation 域（阶段 1-2 补齐：审核申诉） ====
-- 划线口径与其余域一致：写入口全部挂判定，读面默认免鉴权，只有「被读主体明确的个人数据定向读」例外（见 gateway/admin/README.md）。
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('moderation:appeal', 'handle', 'moderation', '审核申诉：处理（把一条审核结论改判到终态，比查任务/结论重）', 0);

-- ==== transcode 域（阶段 1-2 补齐：转码模板） ====
-- 划线口径与其余域一致：写入口全部挂判定，读面默认免鉴权，只有「被读主体明确的个人数据定向读」例外（见 gateway/admin/README.md）。
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('transcode:template', 'create', 'transcode', '转码模板：新建（是所有后续转码任务的参数来源，改一行影响之后每次转码）', 0);

-- ==== danmaku 域（阶段 1-2 补齐：屏蔽词与单条处置） ====
-- 划线口径与其余域一致：写入口全部挂判定，读面默认免鉴权，只有「被读主体明确的个人数据定向读」例外（见 gateway/admin/README.md）。
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('danmaku:block-word', 'update', 'danmaku', '弹幕屏蔽词：新增/停用/删除（事前拦所有内容，影响面是全站弹幕）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('danmaku:item', 'delete', 'danmaku', '弹幕：运营删除单条（事后处置一条内容，reason 落 op_log 审计）', 0);

-- ==== search 域（阶段 1-2 补齐：索引重建与别名切换） ====
-- 划线口径与其余域一致：写入口全部挂判定，读面默认免鉴权，只有「被读主体明确的个人数据定向读」例外（见 gateway/admin/README.md）。
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('search:index', 'rebuild', 'search', '搜索索引：提交重建任务（重灌整份投影，request_id 幂等；索引是投影不是事实源）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('search:alias', 'switch', 'search', '搜索别名：切换到新版本索引（零停机重建的生效开关，切错即全量查询换索引）', 0);

-- ==== risk 域（阶段 1-2 补齐：规则/名单/设备画像/处罚） ====
-- 划线口径与其余域一致：写入口全部挂判定，读面默认免鉴权，只有「被读主体明确的个人数据定向读」例外（见 gateway/admin/README.md）。
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('risk:check', 'run', 'risk', '风控：同步跑一次裁决（按任意主体/动作探测规则，是判定链的在线调试口）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('risk:report', 'create', 'risk', '风控：写行为滑窗计数（伪造上报能把正常用户喂进阈值处罚，故与裁决同挂判定）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('risk:device', 'update', 'risk', '风控设备画像：写入/更新设备与设备-账号关联', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('risk:punishment', 'create', 'risk', '风控处罚：下发（处置真人，operator 与幂等键必填）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('risk:punishment', 'lift', 'risk', '风控处罚：解除（与下发分开授权——能解封的人不该默认能封人）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('risk:rule', 'update', 'risk', '风控规则：新增/更新（判定链上游，改一条影响所有后续裁决）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('risk:list-entry', 'update', 'risk', '风控黑白名单：新增/更新条目（同属判定链上游，但不与规则共用一个点）', 0);

-- ==== comment 域（阶段 1-2 补齐：评论处置） ====
-- 划线口径与其余域一致：写入口全部挂判定，读面默认免鉴权，只有「被读主体明确的个人数据定向读」例外（见 gateway/admin/README.md）。
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('comment:item', 'delete', 'comment', '评论：运营删除（对所有读者消失，由 comment 服务写审计）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('comment:item', 'pin', 'comment', '评论：置顶/取消置顶（改变呈现顺序，与删除方向不同）', 0);

-- ==== notify 域（阶段 1-2 补齐：通知模板与死信） ====
-- 划线口径与其余域一致：写入口全部挂判定，读面默认免鉴权，只有「被读主体明确的个人数据定向读」例外（见 gateway/admin/README.md）。
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('notify:template', 'update', 'notify', '通知模板：存草稿（publish=false 时对外投递没有任何影响）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('notify:template', 'publish', 'notify', '通知模板：发布版本（让一个模板对外生效；能改文案不等于能发给用户）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('notify:template', 'render', 'notify', '通知模板：渲染预览（不落库，但能渲染未发布草稿并看到变量填充结果）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('notify:deadletter', 'retry', 'notify', '通知死信：重投（把已判定失败的消息重新推给供应商通道，真实触达用户）', 0);

-- ==== inbox 域（阶段 1-2 补齐：系统站内信下发与未读纠偏） ====
-- 划线口径与其余域一致：写入口全部挂判定，读面默认免鉴权，只有「被读主体明确的个人数据定向读」例外（见 gateway/admin/README.md）。
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('inbox:message', 'send', 'inbox', '站内信：运营手动下发（全仓最重的用户触达面，接收人规模上限由 inbox 服务判定）', 0);
INSERT IGNORE INTO `op_permission` (`resource`, `action`, `domain`, `description`, `ctime`) VALUES ('inbox:unread', 'recompute', 'inbox', '站内信：重算某用户未读快照并回填缓存（计数纠偏工具，与投递消息不是一回事）', 0);
