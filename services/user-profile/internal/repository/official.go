package repository

// 本文件移植自参考仓库 service/member.go 的官方认证逻辑（SetOfficialDoc/OfficialDoc/Official）
// 与 service/property_review.go、service/user_flag.go 之外的官方认证查询。

import (
	"context"
	"time"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/services/user-profile/model"
	"go-video/services/user-profile/rpc"
)

// SetOfficialDoc 提交官方认证文档（参考 service.SetOfficialDoc）。
// 提交时审核状态强制置为待审核；统一社会信用代码同步写入附加键值表。
func (r *Repository) SetOfficialDoc(ctx context.Context, req *rpc.OfficialDocReq) error {
	doc := &model.OfficialDoc{
		Mid:   req.Mid,
		Name:  req.Name,
		State: model.OfficialStateWait,
		Role:  int8(req.Role),
		Title: req.Title,
		Desc:  req.Desc,
		Extra: model.OfficialExtra{
			Realname:         int8(req.Realname),
			Operator:         req.Operator,
			Telephone:        req.Telephone,
			Email:            req.Email,
			Address:          req.Address,
			Company:          req.Company,
			CreditCode:       req.CreditCode,
			Organization:     req.Organization,
			OrganizationType: req.OrganizationType,
			BusinessLicense:  req.BusinessLicense,
			BusinessScale:    req.BusinessScale,
			BusinessLevel:    req.BusinessLevel,
			BusinessAuth:     req.BusinessAuth,
			Supplement:       req.Supplement,
			Professional:     req.Professional,
			Identification:   req.Identification,
		}.String(),
		SubmitSource: req.SubmitSource,
		SubmitTime:   time.Now().Unix(),
	}
	if !doc.Validate() {
		return ErrRequestErr
	}
	if err := r.officialDocModel.Upsert(ctx, doc); err != nil {
		logx.Errorf("user-profile/official: upsert doc mid=%d err=%v", req.Mid, err)
		return ErrSubmitOfficialDocFailed
	}
	if req.CreditCode != "" {
		if err := r.additModel.Upsert(ctx, req.Mid, "credit_code", req.CreditCode); err != nil {
			logx.Errorf("user-profile/official: upsert addit mid=%d err=%v", req.Mid, err)
		}
	}
	return nil
}

// OfficialDoc 查询官方认证文档（参考 service.OfficialDoc）。
func (r *Repository) OfficialDoc(ctx context.Context, mid int64) (*rpc.OfficialDocInfoReply, error) {
	doc, err := r.officialDocModel.FindOne(ctx, mid)
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, ErrNoOfficialDoc
	}
	extra := model.ParseExtra(doc.Extra)
	return &rpc.OfficialDocInfoReply{
		Mid:              doc.Mid,
		Name:             doc.Name,
		State:            int32(doc.State),
		Role:             int32(doc.Role),
		Title:            doc.Title,
		Desc:             doc.Desc,
		RejectReason:     doc.RejectReason,
		Realname:         int32(extra.Realname),
		Operator:         extra.Operator,
		Telephone:        extra.Telephone,
		Email:            extra.Email,
		Address:          extra.Address,
		Company:          extra.Company,
		CreditCode:       extra.CreditCode,
		Organization:     extra.Organization,
		OrganizationType: extra.OrganizationType,
		BusinessLicense:  extra.BusinessLicense,
		BusinessScale:    extra.BusinessScale,
		BusinessLevel:    extra.BusinessLevel,
		BusinessAuth:     extra.BusinessAuth,
		Supplement:       extra.Supplement,
		Professional:     extra.Professional,
		Identification:   extra.Identification,
	}, nil
}

// Official 查询生效官方认证信息（参考 service.Official，优先内存快照）。
func (r *Repository) Official(ctx context.Context, mid int64) (*rpc.OfficialInfoReply, error) {
	if o := r.Officials()[mid]; o != nil {
		return &rpc.OfficialInfoReply{Role: int32(o.Role), Title: o.Title, Desc: o.Desc}, nil
	}
	o, err := r.officialModel.FindOne(ctx, mid)
	if err != nil {
		return nil, err
	}
	if o == nil {
		return nil, ErrNothingFound
	}
	return &rpc.OfficialInfoReply{Role: int32(o.Role), Title: o.Title, Desc: o.Desc}, nil
}

// AddUserMonitor 添加用户到监控名单（参考 service.AddUserMonitor）。
func (r *Repository) AddUserMonitor(ctx context.Context, mid int64, operator, remark string) error {
	return r.monitorModel.Add(ctx, mid, operator, remark)
}

// IsInMonitor 查询用户是否在监控名单（参考 service.IsInMonitor）。
func (r *Repository) IsInMonitor(ctx context.Context, mid int64) (bool, error) {
	return r.monitorModel.InMonitor(ctx, mid)
}

// PropertyReviewArg 属性审核提交参数。
type PropertyReviewArg struct {
	Mid      int64
	New      string
	State    int8
	Property int8
	Extra    string // JSON 字符串；空则为 {}
}

// AddPropertyReview 添加用户属性变更审核（参考 service.AddPropertyReview）。
// 旧值从当前资料读取（头像取 URL path），提交时归档同属性待审核记录。
func (r *Repository) AddPropertyReview(ctx context.Context, arg *PropertyReviewArg) error {
	base, err := r.baseModel.FindOne(ctx, arg.Mid)
	if err != nil {
		return err
	}
	old := ""
	switch arg.Property {
	case model.ReviewPropertyFace:
		if base != nil {
			old = facePath(base.Face)
		}
	case model.ReviewPropertyName:
		if base != nil {
			old = base.Name
		}
	case model.ReviewPropertySign:
		if base != nil {
			old = base.Sign
		}
	default:
		return ErrRequestErr
	}
	isMonitor, err := r.monitorModel.InMonitor(ctx, arg.Mid)
	if err != nil {
		logx.Errorf("user-profile/property: in monitor mid=%d err=%v", arg.Mid, err)
	}
	if err := r.reviewModel.Archive(ctx, arg.Mid, arg.Property, "", ""); err != nil {
		logx.Errorf("user-profile/property: archive mid=%d property=%d err=%v", arg.Mid, arg.Property, err)
	}
	extra := arg.Extra
	if extra == "" {
		extra = "{}"
	}
	return r.reviewModel.Add(ctx, &model.UserPropertyReview{
		Mid:       arg.Mid,
		Old:       old,
		New:       arg.New,
		State:     arg.State,
		Property:  arg.Property,
		IsMonitor: isMonitor,
		Extra:     extra,
	})
}
