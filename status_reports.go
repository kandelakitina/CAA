package main

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

type statusReportRow struct {
	ID int64
	Title,Status,Internal,Committee,InternalPending,CommitteePending,InternalDeadline,CommitteeDeadline string
	InternalTotal,InternalDone,CommitteeTotal,CommitteeDone int
	Archived,Overdue bool
}

func (app *application) loadStatusReport(c *gin.Context) ([]statusReportRow,error) {
	usr:=c.MustGet("user").(user)
	includeArchived:=usr.Role=="admin" && c.Query("archive")=="1"
	rows,err:=app.db.Query(c.Request.Context(),`
		SELECT q.id,q.title,q.status,q.archived_at IS NOT NULL,
		COALESCE(ir.status,''),COALESCE(ir.outcome,''),COALESCE(TO_CHAR(ir.deadline,'DD.MM.YYYY'),''),
		COALESCE(ip.total,0),COALESCE(ip.done,0),COALESCE(ip.pending,''),
		COALESCE(cr.status,''),COALESCE(cr.outcome,''),COALESCE(TO_CHAR(cr.deadline,'DD.MM.YYYY'),''),
		COALESCE(cp.total,0),COALESCE(cp.done,0),COALESCE(cp.pending,''),
		COALESCE(ir.status='active' AND ir.deadline<CURRENT_DATE,FALSE) OR COALESCE(cr.status='active' AND cr.deadline<CURRENT_DATE,FALSE)
		FROM questions q
		LEFT JOIN LATERAL (SELECT * FROM internal_review_rounds WHERE question_id=q.id ORDER BY id DESC LIMIT 1) ir ON TRUE
		LEFT JOIN LATERAL (SELECT COUNT(*)::INT total,COUNT(*) FILTER(WHERE status='responded')::INT done,
		 STRING_AGG(DISTINCT internal_service,', ' ORDER BY internal_service) FILTER(WHERE status='pending') pending
		 FROM internal_review_requirements WHERE round_id=ir.id AND status<>'superseded') ip ON TRUE
		LEFT JOIN LATERAL (SELECT * FROM committee_vote_rounds WHERE question_id=q.id ORDER BY id DESC LIMIT 1) cr ON TRUE
		LEFT JOIN LATERAL (SELECT COUNT(*)::INT total,COUNT(*) FILTER(WHERE status='voted')::INT done,
		 STRING_AGG(name_snapshot,', ' ORDER BY name_snapshot) FILTER(WHERE status='pending') pending
		 FROM committee_vote_participants WHERE round_id=cr.id) cp ON TRUE
		WHERE ($1 OR q.archived_at IS NULL) AND ($2='' OR q.title ILIKE '%'||$2||'%') AND ($3='' OR q.status=$3)
		AND ($4<>'committee' OR q.status IN ('committee_voting','approved','rejected','no_quorum')
		 OR (q.status='cancelled' AND EXISTS(SELECT 1 FROM committee_vote_rounds WHERE question_id=q.id)))
		AND ($5<>'1' OR (ir.status='active' AND ir.deadline<CURRENT_DATE) OR (cr.status='active' AND cr.deadline<CURRENT_DATE))
		ORDER BY q.updated_at DESC,q.id DESC LIMIT 5001`,includeArchived,strings.TrimSpace(c.Query("q")),c.Query("status"),usr.Role,c.Query("overdue"))
	if err!=nil{return nil,err};defer rows.Close()
	var result []statusReportRow
	for rows.Next() {
		var item statusReportRow;var is,io,cs,co string
		if err:=rows.Scan(&item.ID,&item.Title,&item.Status,&item.Archived,&is,&io,&item.InternalDeadline,&item.InternalTotal,&item.InternalDone,&item.InternalPending,&cs,&co,&item.CommitteeDeadline,&item.CommitteeTotal,&item.CommitteeDone,&item.CommitteePending,&item.Overdue);err!=nil{return nil,err}
		item.Status=questionStatusLabel(item.Status);item.Internal=reportRoundLabel(is,io);item.Committee=reportRoundLabel(cs,co)
		for _,service:=range requiredInternalServices {item.InternalPending=strings.ReplaceAll(item.InternalPending,service,internalServiceLabel(service))}
		if c.Query("overdue")!="1" || item.Overdue {result=append(result,item)}
	}
	return result,rows.Err()
}
func reportRoundLabel(status,outcome string) string {
	if outcome!="" {return questionStatusLabel(outcome)}
	switch status {case "active":return "Идёт согласование";case "completed":return "Завершено";case "cancelled":return "Отменено";default:return "Не запускалось"}
}
func (app *application) showStatusReport(c *gin.Context) {
	items,err:=app.loadStatusReport(c);if err!=nil {c.String(500,"Не удалось загрузить статусы");return}
	if len(items)>5000 {c.String(422,"Более 5000 вопросов. Уточните фильтры");return}
	c.HTML(200,"status-report.html",gin.H{"Title":"Статусы согласования","User":c.MustGet("user"),"CSRFToken":app.templateCSRF(c),"Rows":items,"Query":c.Query("q"),"Status":c.Query("status"),"Archive":c.Query("archive")=="1","Overdue":c.Query("overdue")=="1","ExportURL":"/reports/statuses.csv?"+c.Request.URL.Query().Encode()})
}

// Neutralize spreadsheet formulas, including leading whitespace and tabs.
func csvSafe(value string) string {
	trimmed:=strings.TrimLeft(value," \t\r\n")
	if trimmed!="" && strings.ContainsAny(trimmed[:1],"=+-@") {return "'"+value}
	return value
}
func (app *application) exportStatusReport(c *gin.Context) {
	items,err:=app.loadStatusReport(c);if err!=nil {c.String(500,"Не удалось выгрузить статусы");return}
	if len(items)>5000 {c.String(422,"Более 5000 вопросов. Уточните фильтры");return}
	var buffer bytes.Buffer;buffer.WriteString("\xef\xbb\xbf")
	w:=csv.NewWriter(&buffer);w.Comma=';';w.UseCRLF=true
	_ = w.Write([]string{"ID","Вопрос","Статус вопроса","Архив","Внутреннее согласование","Визы получены","Визы всего","Срок внутреннего","Ожидаются службы","Комитет","Голоса получены","Голоса всего","Срок Комитета","Ожидаются участники","Просрочено"})
	for _,r:=range items {
		fields:=[]string{strconv.FormatInt(r.ID,10),r.Title,r.Status,strconv.FormatBool(r.Archived),r.Internal,strconv.Itoa(r.InternalDone),strconv.Itoa(r.InternalTotal),r.InternalDeadline,r.InternalPending,r.Committee,strconv.Itoa(r.CommitteeDone),strconv.Itoa(r.CommitteeTotal),r.CommitteeDeadline,r.CommitteePending,strconv.FormatBool(r.Overdue)}
		for i:=range fields {fields[i]=csvSafe(fields[i])};_ = w.Write(fields)
	}
	w.Flush();if w.Error()!=nil {c.String(500,"Не удалось сформировать CSV");return}
	c.Header("Content-Disposition",fmt.Sprintf(`attachment; filename="statuses-%s.csv"`,time.Now().Format("2006-01-02")))
	c.Header("Cache-Control","no-store")
	c.Data(http.StatusOK,"text/csv; charset=utf-8",buffer.Bytes())
}
