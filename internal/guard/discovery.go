package guard

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// Only these public fields leave discovery. Never include credentials, proxy
// passwords or arbitrary account metadata in plugin status / iframe messages.
type AccountOption struct {
	GroupIDs          []int64 `json:"group_ids"`
	ID                int64   `json:"id"`
	Name              string  `json:"name"`
	Type              string  `json:"type"`
	Status            string  `json:"status"`
	Schedulable       bool    `json:"schedulable"`
	Supported         bool    `json:"supported"`
	SuggestedModel    string  `json:"suggested_model"`
	SuggestedExpected string  `json:"suggested_expected"`
}

func (a *AdminClient) ListAccounts(ctx context.Context) ([]AccountOption, error) {
	if a.Key == "" {
		return nil, errors.New("请先保存管理员访问密钥，再加载账号")
	}
	out := []AccountOption{}
	seen := map[int64]bool{}
	expectedTotal := -1
	for page := 1; page <= 100; page++ {
		var data struct {
			Items []struct {
				GroupIDs    []int64 `json:"group_ids"`
				ID          int64   `json:"id"`
				Name        string  `json:"name"`
				Platform    string  `json:"platform"`
				Type        string  `json:"type"`
				Status      string  `json:"status"`
				Schedulable bool    `json:"schedulable"`
				ParentID    *int64  `json:"parent_account_id"`
			} `json:"items"`
			Total *int `json:"total"`
			Pages int  `json:"pages"`
			Page  int  `json:"page"`
		}
		path := fmt.Sprintf("/api/v1/admin/accounts?platform=openai&page=%d&page_size=100&sort_by=id&sort_order=asc", page)
		if err := a.call(ctx, "GET", path, nil, &data); err != nil {
			return nil, err
		}
		if data.Page != page || data.Total == nil || *data.Total < 0 || data.Items == nil {
			return nil, errors.New("账号列表分页格式不兼容")
		}
		if expectedTotal < 0 {
			expectedTotal = *data.Total
		}
		if *data.Total != expectedTotal {
			return nil, errors.New("账号分页总数变化；保留原监测目标")
		}
		for _, x := range data.Items {
			if x.ID <= 0 || seen[x.ID] {
				return nil, errors.New("账号列表在分页期间发生变化，请重新加载")
			}
			seen[x.ID] = true
			if x.Platform != "openai" {
				continue
			}
			option := AccountOption{GroupIDs: x.GroupIDs, ID: x.ID, Name: x.Name, Type: x.Type, Status: x.Status, Schedulable: x.Schedulable, Supported: x.ParentID == nil && (x.Type == "oauth" || x.Type == "apikey"), SuggestedModel: "gpt-6.1-terra", SuggestedExpected: "gpt-6.1-terra"}
			if x.Type == "apikey" {
				option.SuggestedModel = "gpt-6.1-sol"
				option.SuggestedExpected = "gpt-6-astra"
			}
			out = append(out, option)
		}
		if len(seen) > *data.Total {
			return nil, errors.New("账号分页数量超出总数")
		}
		if len(seen) == *data.Total {
			sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
			return out, nil
		}
		if len(data.Items) == 0 || (data.Pages > 0 && page >= data.Pages) {
			return nil, errors.New("账号列表不完整，请重新加载")
		}
	}
	return nil, errors.New("账号列表超过 10000 条，请缩小管理密钥授权范围")
}
