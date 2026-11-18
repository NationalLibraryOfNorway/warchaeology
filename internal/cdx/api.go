/*
 * Copyright 2021 National Library of Norway.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *       http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package index

import (
	"context"

	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx/schema"

	"github.com/nationallibraryofnorway/whatwg-url/url"
)

type DateRange interface {
	Contains(int64) bool
}

type Filter interface {
	Eval(*schema.Cdx) bool
}

type Sort int

const (
	SortNone    Sort = iota
	SortDesc         // largest to smallest alphabetically
	SortAsc          // smallest to largest alphabetically
	SortClosest      // closest in time
)

type MatchType int

const (
	MatchTypeExact MatchType = iota
	MatchTypePrefix
	MatchTypeHost
	MatchTypeDomain
	MatchTypeVerbatim
)

type Request interface {
	Url() *url.Url
	OriginalURI() string
	Ssurt() string
	Sort() Sort
	DateRange() DateRange
	Filter() Filter
	Limit() int
	Closest() string
	MatchType() MatchType
}

type FileAPI interface {
	GetFileInfo(ctx context.Context, filename string) (*schema.FileInfo, error)
	ListFileInfo(context.Context, Request, chan<- FileInfoResponse) error
}

type IdAPI interface {
	GetStorageRef(ctx context.Context, warcId string) (string, error)
	ListStorageRef(context.Context, Request, chan<- IdResponse) error
}

type CdxAPI interface {
	Search(context.Context, Request, chan<- CdxResponse) error
}

type API interface {
	CdxAPI
	FileAPI
	IdAPI
	ReportAPI
}

type FileInfoResponse interface {
	GetFileInfo() *schema.FileInfo
	GetError() error
}

type CdxResponse interface {
	GetCdx() *schema.Cdx
	GetError() error
}

type IdResponse interface {
	GetId() string
	GetValue() string
	GetError() error
}

type ReportResponse interface {
	GetReport() *schema.Report
	GetError() error
}

type ReportAPI interface {
	CreateReport(context.Context, Request) (*schema.Report, error)
	ListReports(context.Context, Request, chan<- ReportResponse) error
	GetReport(context.Context, string) (*schema.Report, error)
	CancelReport(context.Context, string) error
	DeleteReport(context.Context, string) error
}

type ReportGenerator interface {
	CdxAPI
	AddTask(string, context.CancelFunc)
	DeleteTask(string)
	SaveReport(context.Context, *schema.Report) error
}
