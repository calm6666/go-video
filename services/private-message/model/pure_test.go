package model

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestPairKeyNormalized 锁死 pair_key 的规范化规则：
// pm_conversation.uniq_pair_key 是「建会话」唯一的幂等落点，(a,b) 与 (b,a) 必须同值，
// 否则同一对用户会开出两个会话，未读游标与 seq 从此分叉且无法在线修复。
func TestPairKeyNormalized(t *testing.T) {
	cases := []struct {
		name string
		a, b int64
		want string
	}{
		{"升序", 7, 42, "7:42"},
		{"降序自动交换", 42, 7, "7:42"},
		{"相等", 42, 42, "42:42"},
		{"零值占位", 0, 42, "0:42"},
		{"负数（非法入参也必须可预测）", -1, 42, "-1:42"},
		{"int64 上界", 9223372036854775807, 1, "1:9223372036854775807"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := PairKey(c.a, c.b); got != c.want {
				t.Errorf("PairKey(%d,%d)=%q want %q", c.a, c.b, got, c.want)
			}
			if PairKey(c.a, c.b) != PairKey(c.b, c.a) {
				t.Errorf("PairKey 不满足交换不变量：%d/%d", c.a, c.b)
			}
		})
	}
}

// TestPairKeyNoAmbiguity 冒号分隔必须避免「拼接歧义」：
// 无分隔符时 (1,23) 与 (12,3) 会撞成同一个键，把两个用户的会话合并到一起，属隐私事故。
func TestPairKeyNoAmbiguity(t *testing.T) {
	seen := map[string][2]int64{}
	pairs := [][2]int64{{1, 23}, {12, 3}, {1, 2}, {12, 30}, {123, 0}, {0, 1230}, {7, 7}}
	for _, p := range pairs {
		k := PairKey(p[0], p[1])
		if prev, ok := seen[k]; ok && prev != p {
			t.Fatalf("pair_key 冲突：%v 与 %v 都是 %q", prev, p, k)
		}
		seen[k] = p
	}
	if PairKey(1, 23) == PairKey(12, 3) {
		t.Fatal("(1,23) 与 (12,3) 必须不同键")
	}
}

// TestPairKeyFitsColumn pair_key 列是 VARCHAR(64)：
// 两侧 int64 十进制 + 冒号的最坏长度必须留有余量，否则线上会因「值过长被截断」丢幂等性。
func TestPairKeyFitsColumn(t *testing.T) {
	worst := PairKey(9223372036854775807, -9223372036854775808)
	if utf8.RuneCountInString(worst) >= 64 {
		t.Fatalf("pair_key 最坏长度 %d 逼近 VARCHAR(64)，必须先扩列再上线", utf8.RuneCountInString(worst))
	}
}

// TestEnumNumberingPinned 状态/类型编号同时出现在 pm_*.sql 的列注释、rpc 枚举与事件 payload 里，
// 重排不会编译报错，但会让历史行的状态被误读（例如把「已撤回」当成「正常」重新露出正文）。
func TestEnumNumberingPinned(t *testing.T) {
	cases := []struct {
		name string
		got  int32
		want int32
	}{
		{"MsgStateNormal", MsgStateNormal, 1},
		{"MsgStatePendingReview", MsgStatePendingReview, 2},
		{"MsgStateWithdrawn", MsgStateWithdrawn, 3},
		{"MsgStateRejected", MsgStateRejected, 4},
		{"MsgStateDeleted", MsgStateDeleted, 5},
		{"MsgTypeText", MsgTypeText, 1},
		{"MsgTypeShare", MsgTypeShare, 5},
		{"ConversationStateNormal", ConversationStateNormal, 1},
		{"ConversationStateFrozen", ConversationStateFrozen, 2},
		{"WithdrawSourceSender", WithdrawSourceSender, 1},
		{"WithdrawSourceAdmin", WithdrawSourceAdmin, 4},
		{"AllowFromAnyone", AllowFromAnyone, 1},
		{"AllowFromNone", AllowFromNone, 4},
		{"ReportStatePending", ReportStatePending, 1},
		{"ReportStateDismissed", ReportStateDismissed, 3},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s 编号被改动：got=%d want=%d", c.name, c.got, c.want)
		}
	}
}

// TestValidatorsRejectUnspecified 三个 Valid* 必须拒绝 0（UNSPECIFIED）与区间外取值：
// 0 在 proto3 里是「字段没填」，当成合法值就等于放行未初始化的状态迁移。
func TestValidatorsRejectUnspecified(t *testing.T) {
	validators := map[string]func(int32) bool{
		"ValidMsgType":        ValidMsgType,
		"ValidAllowFrom":      ValidAllowFrom,
		"ValidWithdrawSource": ValidWithdrawSource,
	}
	invalid := []int32{0, -1, 6, 99, 255}
	for name, ok := range validators {
		for _, v := range invalid {
			if ok(v) {
				t.Errorf("%s 接受了非法取值 %d（0=UNSPECIFIED 一律拒绝）", name, v)
			}
		}
	}
	for _, v := range []int32{MsgTypeText, MsgTypeImage, MsgTypeAudio, MsgTypeVideo, MsgTypeShare} {
		if !ValidMsgType(v) {
			t.Errorf("ValidMsgType(%d) 应为 true", v)
		}
	}
	for _, v := range []int32{AllowFromAnyone, AllowFromFollowed, AllowFromMutual, AllowFromNone} {
		if !ValidAllowFrom(v) {
			t.Errorf("ValidAllowFrom(%d) 应为 true", v)
		}
	}
	for _, v := range []int32{WithdrawSourceSender, WithdrawSourceReceiver, WithdrawSourceModeration, WithdrawSourceAdmin} {
		if !ValidWithdrawSource(v) {
			t.Errorf("ValidWithdrawSource(%d) 应为 true", v)
		}
	}
}

// TestDefaultUserSettingFallback 站点级默认值越界时必须退到「最保守但不阻断」的取值，
// 而不是把非法值写进 pm_user_setting.allow_from（列是 TINYINT，非法值会静默落库）。
func TestDefaultUserSettingFallback(t *testing.T) {
	cases := []struct {
		name  string
		given int32
		want  int32
	}{
		{"正常默认", AllowFromMutual, AllowFromMutual},
		{"零值兜底", 0, AllowFromAnyone},
		{"越界兜底", 9, AllowFromAnyone},
		{"负数兜底", -3, AllowFromAnyone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := DefaultUserSetting(101, c.given)
			if s.AllowFrom != c.want {
				t.Errorf("AllowFrom=%d want %d", s.AllowFrom, c.want)
			}
			if s.Mid != 101 {
				t.Errorf("Mid=%d want 101", s.Mid)
			}
			// 关键词过滤默认开启：关闭只允许显式设置，缺省行不能自带「不过滤」。
			if s.KeywordFilter != 1 {
				t.Errorf("KeywordFilter=%d want 1", s.KeywordFilter)
			}
			if !ValidAllowFrom(s.AllowFrom) {
				t.Errorf("缺省值 %d 本身非法", s.AllowFrom)
			}
		})
	}
}

// TestAcceptsUnknownSenderFailClosed 关系数据拿不到时的判定必须是 fail-closed：
// 只有「任何人可收 + 不拒陌生人」才放行，nil（用户没设置过）与任何门槛都拒。
func TestAcceptsUnknownSenderFailClosed(t *testing.T) {
	cases := []struct {
		name string
		s    *UserSetting
		want bool
	}{
		{"nil 设置行", nil, false},
		{"空结构", &UserSetting{}, false},
		{"任何人且不强拒陌生人", &UserSetting{AllowFrom: AllowFromAnyone}, true},
		{"任何人但拒陌生人", &UserSetting{AllowFrom: AllowFromAnyone, RejectStranger: 1}, false},
		{"仅关注", &UserSetting{AllowFrom: AllowFromFollowed}, false},
		{"仅互关", &UserSetting{AllowFrom: AllowFromMutual}, false},
		{"关闭私信", &UserSetting{AllowFrom: AllowFromNone}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.s.AcceptsUnknownSender(); got != c.want {
				t.Errorf("AcceptsUnknownSender()=%v want %v", got, c.want)
			}
		})
	}
}

// TestConversationMemberUnreadFloor 未读投影列可能因历史脏数据或并发扣减变负，
// 角标对外绝不能出现负数（客户端会渲染成异常值）。
func TestConversationMemberUnreadFloor(t *testing.T) {
	cases := []struct {
		name string
		m    *ConversationMember
		want int64
	}{
		{"nil 行", nil, 0},
		{"零", &ConversationMember{UnreadCount: 0}, 0},
		{"负数投影", &ConversationMember{UnreadCount: -7}, 0},
		{"正常", &ConversationMember{UnreadCount: 3}, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.m.Unread(); got != c.want {
				t.Errorf("Unread()=%d want %d", got, c.want)
			}
		})
	}
}

// TestMessageHasPlainContent 区分「还能解密」与「留存到期已清理」：
// 已清理的行必须走占位文案而不是报解密失败，否则用户看到的是错误而不是「内容已删除」。
func TestMessageHasPlainContent(t *testing.T) {
	cases := []struct {
		name string
		m    *Message
		want bool
	}{
		{"nil", nil, false},
		{"有密文", &Message{ContentCipher: []byte("nonce||ct")}, true},
		{"密文为空", &Message{}, false},
		{"已清理", &Message{ContentCipher: []byte("x"), ContentPurged: ContentPurgedYes}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.m.HasPlainContent(); got != c.want {
				t.Errorf("HasPlainContent()=%v want %v", got, c.want)
			}
		})
	}
}

// TestGuardsRejectBeforeTouchingDB 参数守卫必须在发起任何 SQL 之前返回：
// 这里刻意用 nil 连接构造 model——一旦某个方法漏了校验就会 panic，
// 等于用测试锁住「非法入参不消耗数据库连接、不产生写入」（AGENTS.md §5/§9）。
func TestGuardsRejectBeforeTouchingDB(t *testing.T) {
	ctx := context.Background()
	conversations := NewConversationModel(nil)
	members := NewConversationMemberModel(nil)
	messages := NewMessageModel(nil)
	settings := NewUserSettingModel(nil)
	reports := NewReportModel(nil)
	withdrawLogs := NewWithdrawLogModel(nil)

	t.Run("消息写入缺幂等键与会话", func(t *testing.T) {
		if _, _, err := messages.Insert(ctx, nil, &Message{}); !errors.Is(err, ErrConversationNotFound) {
			t.Errorf("Insert(空会话) err=%v want ErrConversationNotFound", err)
		}
		_, _, err := messages.Insert(ctx, nil, &Message{ConversationID: 1, Seq: 1})
		if !errors.Is(err, ErrClientMsgIDRequired) {
			t.Errorf("Insert(缺 client_msg_id) err=%v want ErrClientMsgIDRequired", err)
		}
		if _, err := messages.FindByClientMsgID(ctx, 1, ""); !errors.Is(err, ErrClientMsgIDRequired) {
			t.Errorf("FindByClientMsgID(空键) err=%v want ErrClientMsgIDRequired", err)
		}
	})

	t.Run("状态迁移必须给定来源态", func(t *testing.T) {
		if _, err := messages.MarkState(ctx, nil, 1, nil, MsgStateNormal, 0, ""); !errors.Is(err, ErrInvalidStateTransition) {
			t.Errorf("MarkState(from 为空) err=%v want ErrInvalidStateTransition", err)
		}
	})

	t.Run("seq 分配必须在事务内", func(t *testing.T) {
		if _, err := conversations.AllocateSeq(ctx, nil, 1); err == nil ||
			!strings.Contains(err.Error(), "transaction") {
			t.Errorf("AllocateSeq(nil session) err=%v，要求拒绝无事务调用", err)
		}
	})

	t.Run("分页与批量上限先于查询", func(t *testing.T) {
		if _, err := messages.ListBeforeSeq(ctx, 1, 0, 0); !errors.Is(err, ErrInvalidPage) {
			t.Errorf("ListBeforeSeq(ps=0) err=%v want ErrInvalidPage", err)
		}
		if _, err := messages.ListPurgeCandidates(ctx, 1, 0); !errors.Is(err, ErrInvalidPage) {
			t.Errorf("ListPurgeCandidates(limit=0) err=%v want ErrInvalidPage", err)
		}
		if _, err := members.ListByMid(ctx, ListMembersOptions{Mid: 1}); !errors.Is(err, ErrInvalidPage) {
			t.Errorf("ListByMid(ps=0) err=%v want ErrInvalidPage", err)
		}
		if _, err := reports.ListByCursor(ctx, 0, 0, 0, 0); !errors.Is(err, ErrInvalidPage) {
			t.Errorf("ListByCursor(ps=0) err=%v want ErrInvalidPage", err)
		}
		if _, err := reports.ListPending(ctx, 0); !errors.Is(err, ErrInvalidPage) {
			t.Errorf("ListPending(ps=0) err=%v want ErrInvalidPage", err)
		}
		if _, err := withdrawLogs.ListByOperator(ctx, 1, 0); !errors.Is(err, ErrInvalidPage) {
			t.Errorf("ListByOperator(ps=0) err=%v want ErrInvalidPage", err)
		}
	})

	t.Run("空集合短路不生成 IN () 语句", func(t *testing.T) {
		if n, err := messages.PurgeByIDs(ctx, nil); err != nil || n != 0 {
			t.Errorf("PurgeByIDs(nil) = (%d,%v) want (0,nil)", n, err)
		}
		if m, err := messages.FindByIDs(ctx, nil); err != nil || len(m) != 0 {
			t.Errorf("FindByIDs(nil) = (%v,%v) want 空 map", m, err)
		}
		if m, err := conversations.FindByIDs(ctx, nil); err != nil || len(m) != 0 {
			t.Errorf("FindByIDs(nil) = (%v,%v) want 空 map", m, err)
		}
		if m, err := settings.FindByMids(ctx, nil); err != nil || len(m) != 0 {
			t.Errorf("FindByMids(nil) = (%v,%v) want 空 map", m, err)
		}
		if m, err := members.ListPeersByMid(ctx, 1, nil); err != nil || len(m) != 0 {
			t.Errorf("ListPeersByMid(nil) = (%v,%v) want 空 map", m, err)
		}
		if err := members.Ensure(ctx, nil, nil); err != nil {
			t.Errorf("Ensure(nil rows) err=%v want nil", err)
		}
	})

	t.Run("偏好与审计行的主键校验", func(t *testing.T) {
		if err := settings.Upsert(ctx, nil); !errors.Is(err, ErrInvalidMid) {
			t.Errorf("Upsert(nil) err=%v want ErrInvalidMid", err)
		}
		if err := settings.Upsert(ctx, &UserSetting{Mid: 1, AllowFrom: 0}); !errors.Is(err, ErrInvalidAllowFrom) {
			t.Errorf("Upsert(allow_from=0) err=%v want ErrInvalidAllowFrom", err)
		}
		if err := withdrawLogs.Insert(ctx, nil, &WithdrawLog{MsgID: 1, Source: 0}); !errors.Is(err, ErrInvalidStateTransition) {
			t.Errorf("Insert(source=UNSPECIFIED) err=%v want ErrInvalidStateTransition", err)
		}
		if err := withdrawLogs.Insert(ctx, nil, nil); !errors.Is(err, ErrMessageNotFound) {
			t.Errorf("Insert(nil log) err=%v want ErrMessageNotFound", err)
		}
		if _, _, err := reports.Insert(ctx, &Report{}); !errors.Is(err, ErrInvalidMid) {
			t.Errorf("Report.Insert(缺主键) err=%v want ErrInvalidMid", err)
		}
		if _, err := reports.FindByHandleKey(ctx, ""); !errors.Is(err, ErrIdempotencyKeyRequired) {
			t.Errorf("FindByHandleKey(空键) err=%v want ErrIdempotencyKeyRequired", err)
		}
		if err := reports.MarkHandled(ctx, 1, ReportStateHandled, 2, "note", ""); !errors.Is(err, ErrIdempotencyKeyRequired) {
			t.Errorf("MarkHandled(空幂等键) err=%v want ErrIdempotencyKeyRequired", err)
		}
	})
}

// TestSentinelErrorsDistinct 哨兵错误文案两两不同且带服务前缀：
// gateway/app 按 errors.Is 映射响应信封 code，文案重复会让排障与映射都失真。
func TestSentinelErrorsDistinct(t *testing.T) {
	all := []error{
		ErrNotImplemented, ErrInvalidMid, ErrSelfConversation, ErrConversationNotFound,
		ErrNotConversationMember, ErrMessageNotFound, ErrContentEmpty, ErrContentTooLong,
		ErrMediaRefRequired, ErrInvalidMsgType, ErrClientMsgIDRequired, ErrIdempotencyKeyRequired,
		ErrInvalidCursor, ErrInvalidPage, ErrPsTooLarge, ErrInvalidStateTransition,
		ErrConcurrentUpdate, ErrWithdrawWindowClosed, ErrWithdrawForbidden, ErrOperatorRequired,
		ErrReportNotFound, ErrInvalidReportAction, ErrInvalidVerdict, ErrInvalidAllowFrom,
		ErrBlockedByPeer, ErrRiskDenied, ErrConversationFrozen, ErrSocialGraphNotConfigured,
		ErrRiskControlNotConfigured, ErrModerationNotConfigured, ErrCipherKeyMissing,
		ErrDecryptFailed, ErrBatchLimitTooLarge,
	}
	seen := map[string]error{}
	for _, e := range all {
		if e == nil {
			t.Fatal("存在未赋值的哨兵错误")
		}
		msg := e.Error()
		if !strings.HasPrefix(msg, "private-message: ") {
			t.Errorf("%q 缺少服务名前缀，日志无法定位归属服务", msg)
		}
		if prev, ok := seen[msg]; ok {
			t.Errorf("错误文案重复：%q 与 %q", prev, e)
		}
		seen[msg] = e
	}
	if len(all) < 30 {
		t.Fatalf("哨兵错误数量异常：%d", len(all))
	}
	// sql.ErrNoRows 不得外泄给调用方：model 层统一转成 (nil, nil) 或语义错误。
	if errors.Is(ErrConversationNotFound, sql.ErrNoRows) {
		t.Fatal("ErrConversationNotFound 不应包装 sql.ErrNoRows")
	}
}
