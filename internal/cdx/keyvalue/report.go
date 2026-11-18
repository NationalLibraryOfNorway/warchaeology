package keyvalue

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/google/uuid"
	index "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx/schema"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/server/api"
	"golang.org/x/net/publicsuffix"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type ReportGenerator struct {
	index.ReportGenerator

	Id              string
	UpdateInterval  time.Duration
	UpdateThreshold int
}

func NewReportGenerator(g index.ReportGenerator) (*ReportGenerator, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	return &ReportGenerator{
		Id:              id.String(),
		UpdateInterval:  5 * time.Second,
		UpdateThreshold: 100000,
		ReportGenerator: g,
	}, nil
}

// func decode(any interface{}) (*structpb.Struct, error) {
// 	var dataMap map[string]interface{}
// 	err := mapstructure.Decode(any, &dataMap)
// 	if err != nil {
// 		return nil, err
// 	}
// 	return structpb.NewStruct(dataMap)
// }

func mapRequestToStructPb(req index.Request) (*structpb.Struct, error) {
	searchReq, ok := req.(*api.SearchRequest)
	if !ok {
		panic("assert: req (index.Request) is not a *api.SearchRequest")
	}
	values := searchReq.Values
	m := make(map[string]any, len(values))
	for k, v := range values {
		if len(v) == 1 {
			m[k] = v[0]
		} else {
			m[k] = strings.Join(v, ",")
		}
	}
	return structpb.NewStruct(m)
}

func (r ReportGenerator) Generate(ctx context.Context, req index.Request) (*schema.Report, error) {
	if r.Id == "" {
		return nil, fmt.Errorf("report generator id is empty")
	}

	query, err := mapRequestToStructPb(req)
	if err != nil {
		return nil, err
	}

	report := &schema.Report{
		Id:        r.Id,
		StartTime: timestamppb.New(time.Now()),
		Status:    schema.Report_PENDING,
		Query:     query,
	}
	err = r.SaveReport(ctx, report)
	if err != nil {
		return nil, err
	}

	initial := proto.Clone(report).(*schema.Report)
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	r.AddTask(r.Id, cancel)
	go func() {
		var err error
		defer cancel()
		defer r.DeleteTask(r.Id)

		report.Status = schema.Report_RUNNING
		reportData := &schema.ReportData{
			CountByStatusCode:  make(map[string]uint64),
			CountByRecordType:  make(map[string]uint64),
			CountByContentType: make(map[string]uint64),
			CountByScheme:      make(map[string]uint64),
		}
		report.Data = reportData

		defer func() {
			if err != nil {
				report.Error = err.Error()
				report.Status = schema.Report_FAILED
			} else {
				report.Progress = ""
				report.Status = schema.Report_COMPLETED
			}
			report.EndTime = timestamppb.New(time.Now())
			report.Duration = durationpb.New(report.GetEndTime().AsTime().Sub(report.GetStartTime().AsTime()))

			ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			err = r.SaveReport(ctx, report)
			if err != nil {
				slog.Error("Failed to save report", "error", err)
			}
		}()

		results := make(chan index.CdxResponse)
		err = r.Search(ctx, req, results)
		if err != nil {
			return
		}

		var (
			resp                       CdxResponse
			key                        CdxKey
			cdx                        *schema.Cdx
			target, prevTarget         string
			surtDomain, prevSurtDomain string
			ts, prevTs                 time.Time
			path, prevPath             string
			contentType                string
			ok                         bool
		)

		updateCount := 0
		tick := time.NewTicker(r.UpdateInterval)
		defer tick.Stop()
		for result := range results {
			tock := false
			select {
			case <-ctx.Done():
				err = ctx.Err()
				return
			case <-tick.C:
				tock = true
			default:
			}
			resp, ok = result.(CdxResponse)
			if !ok {
				panic("assert: result (index.CdxResponse) is not a keyvalue.CdxResponse")
			}
			key = resp.Key
			cdx = resp.Value

			report.Progress = string(key)

			if tock || updateCount%r.UpdateThreshold == 0 {
				report.Duration = durationpb.New(time.Since(report.GetStartTime().AsTime()))
				updateCount = 0

				err = r.SaveReport(ctx, report)
				if err != nil {
					return
				}
			}

			reportData.NrOfRecords++
			updateCount++

			err = result.GetError()
			if err != nil {
				return
			}

			// Update surtDomain
			prevSurtDomain = surtDomain
			surtDomain = key.Domain()

			if surtDomain != prevSurtDomain {
				// Increment number of domains
				reportData.NrOfDomains++

				// Update target
				domain := deSurtDomain(surtDomain)
				prevTarget = target
				target, err = publicsuffix.EffectiveTLDPlusOne(domain)
				if err != nil {
					slog.Warn("failed to get effective tld plus one", "domain", domain)
					err = nil
					target = domain
				}
				if prevTarget != target {
					// Increment number of targets
					reportData.NrOfTargets++
				}
			}

			prevPath = path
			path = key.Path()

			prevTs = ts
			ts = key.Time()

			// A target capture is a capture of a url with path "/"
			if path == "/" {
				// Same domain and path within 5 seconds
				// is not counted as a new target capture
				// to avoid counting immediate redirects.
				if prevPath == path &&
					surtDomain == prevSurtDomain &&
					ts.Sub(prevTs) <= 5*time.Second {
					// Count immediate redirects as a single target capture.
				} else {
					reportData.NrOfTargetCaptures++
				}
			}

			// Different path or domain means new url
			// (deliberately ignoring scheme, port and userinfo).
			if path != prevPath || surtDomain != prevSurtDomain {
				reportData.NrOfUrls++
			}

			// Group content type by mime type
			contentType = strings.SplitN(cdx.GetMct(), ";", 2)[0]

			reportData.CountByStatusCode[strconv.Itoa(int(cdx.GetHsc()))]++
			reportData.CountByRecordType[cdx.GetSrt()]++
			reportData.CountByContentType[contentType]++
			scheme, _, _ := strings.Cut(cdx.GetUri(), ":")
			reportData.CountByScheme[scheme]++
			reportData.ContentLength += uint64(cdx.GetCle())
			reportData.PayloadLength += uint64(cdx.GetPle())
			reportData.RecordLength += uint64(cdx.GetRle())
		}
	}()

	return initial, nil
}
