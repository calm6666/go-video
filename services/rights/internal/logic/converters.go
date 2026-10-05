package logic

import (
	"go-video/services/rights/model"
	"go-video/services/rights/rpc"
)

// contractToRPC 把 model.RightsContract 转换为 rpc.Contract。
func contractToRPC(c *model.RightsContract) *rpc.Contract {
	if c == nil {
		return nil
	}
	return &rpc.Contract{
		ContractId: c.ContractID,
		OwnerId:    c.OwnerID,
		Title:      c.Title,
		SignDate:   c.SignDate,
		StartDate:  c.StartDate,
		EndDate:    c.EndDate,
		Regions:    model.RegionsFromCSV(c.RegionsCSV),
		State:      contractStateToRPC(c.State),
		Ctime:      c.Ctime,
		Mtime:      c.Mtime,
	}
}

// windowToRPC 把 model.RightsWindow 转换为 rpc.Window。
func windowToRPC(w *model.RightsWindow) *rpc.Window {
	if w == nil {
		return nil
	}
	return &rpc.Window{
		WindowId:    w.WindowID,
		ContractId:  w.ContractID,
		ContentId:   w.ContentID,
		ContentType: contentTypeToRPC(w.ContentType),
		Region:      w.Region,
		StartTime:   w.StartTime,
		EndTime:     w.EndTime,
		State:       windowStateToRPC(w.State),
		Ctime:       w.Ctime,
		Mtime:       w.Mtime,
	}
}

func contractStateToRPC(s int32) rpc.ContractState {
	switch s {
	case model.ContractStateActive:
		return rpc.ContractState_CONTRACT_STATE_ACTIVE
	case model.ContractStateTerminated:
		return rpc.ContractState_CONTRACT_STATE_TERMINATED
	default:
		return rpc.ContractState_CONTRACT_STATE_UNSPECIFIED
	}
}

func windowStateToRPC(s int32) rpc.WindowState {
	switch s {
	case model.WindowStateActive:
		return rpc.WindowState_WINDOW_STATE_ACTIVE
	case model.WindowStateExpired:
		return rpc.WindowState_WINDOW_STATE_EXPIRED
	case model.WindowStateRevoked:
		return rpc.WindowState_WINDOW_STATE_REVOKED
	default:
		return rpc.WindowState_WINDOW_STATE_UNSPECIFIED
	}
}

func contentTypeToRPC(t int32) rpc.ContentType {
	switch t {
	case model.ContentTypePGC:
		return rpc.ContentType_CONTENT_TYPE_PGC
	case model.ContentTypeUGC:
		return rpc.ContentType_CONTENT_TYPE_UGC
	default:
		return rpc.ContentType_CONTENT_TYPE_UNSPECIFIED
	}
}
