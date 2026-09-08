package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

const resetTables = `protocol_questions, protocols, protocol_sequences, committee_vote_attachments,
committee_votes, committee_vote_participants, committee_vote_round_files, committee_vote_rounds,
internal_visa_attachments, internal_review_visas, internal_review_requirements, decision_text_revisions,
internal_review_rounds, question_file_versions, question_files, questions,
approval_participants, approval_rounds, document_versions, documents, audit_events`

func (app *application) migrateManagement(ctx context.Context) error {
	_, err := app.db.Exec(ctx, `
		ALTER TABLE questions ADD COLUMN IF NOT EXISTS archived_at TIMESTAMPTZ;
		ALTER TABLE documents ADD COLUMN IF NOT EXISTS archived_at TIMESTAMPTZ;
		CREATE TABLE IF NOT EXISTS storage_cleanup (
			id BIGSERIAL PRIMARY KEY, endpoint TEXT NOT NULL, bucket TEXT NOT NULL,
			object_key TEXT NOT NULL, version_id TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), completed_at TIMESTAMPTZ,
			attempts INTEGER NOT NULL DEFAULT 0,
			UNIQUE(endpoint,bucket,object_key,version_id)
		);
		CREATE OR REPLACE FUNCTION prevent_archived_changes() RETURNS TRIGGER AS $$
		BEGIN
			IF OLD.archived_at IS NOT NULL THEN RAISE EXCEPTION 'Archived records are read-only'; END IF;
			RETURN NEW;
		END; $$ LANGUAGE plpgsql;
		DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname='questions_archive_guard' AND tgrelid='questions'::regclass) THEN
				CREATE TRIGGER questions_archive_guard BEFORE UPDATE ON questions FOR EACH ROW EXECUTE FUNCTION prevent_archived_changes();
			END IF;
			IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname='documents_archive_guard' AND tgrelid='documents'::regclass) THEN
				CREATE TRIGGER documents_archive_guard BEFORE UPDATE ON documents FOR EACH ROW EXECUTE FUNCTION prevent_archived_changes();
			END IF;
		END $$;
	`)
	return err
}

type adminSnapshot struct { Questions, Documents, Users, ActiveRounds, Files, PendingCleanup int; AuditID int64 }
type adminMaterial struct { ID int64; Kind,Title,Status string; Archived bool }

func loadAdminSnapshot(ctx context.Context, db revisionPlanDB, actorID int64) (adminSnapshot,error) {
	var s adminSnapshot
	err:=db.QueryRow(ctx,`SELECT
		(SELECT COUNT(*) FROM questions), (SELECT COUNT(*) FROM documents),
		(SELECT COUNT(*) FROM users WHERE id<>$1),
		(SELECT COUNT(*) FROM internal_review_rounds WHERE status='active')+(SELECT COUNT(*) FROM committee_vote_rounds WHERE status='active')+(SELECT COUNT(*) FROM approval_rounds WHERE status='active'),
		(SELECT COUNT(*) FROM question_file_versions)+(SELECT COUNT(*) FROM document_versions)+(SELECT COUNT(*) FROM internal_visa_attachments)+(SELECT COUNT(*) FROM committee_vote_attachments),
		(SELECT COUNT(*) FROM storage_cleanup WHERE completed_at IS NULL),
		(SELECT COALESCE(MAX(id),0) FROM audit_events)`,actorID).Scan(&s.Questions,&s.Documents,&s.Users,&s.ActiveRounds,&s.Files,&s.PendingCleanup,&s.AuditID)
	return s,err
}

func (app *application) adminDashboard(c *gin.Context) {
	actor:=c.MustGet("user").(user)
	s,err:=loadAdminSnapshot(c.Request.Context(),app.db,actor.ID)
	if err!=nil {c.String(500,"Не удалось загрузить сводку");return}
	rows,err:=app.db.Query(c.Request.Context(),`SELECT id,'question',title,status,archived_at IS NOT NULL FROM questions
		UNION ALL SELECT id,'document',title,status,archived_at IS NOT NULL FROM documents ORDER BY 1 DESC LIMIT 200`)
	if err!=nil {c.String(500,"Не удалось загрузить материалы");return};defer rows.Close()
	var items []adminMaterial
	for rows.Next() {var item adminMaterial;if err:=rows.Scan(&item.ID,&item.Kind,&item.Title,&item.Status,&item.Archived);err!=nil {c.String(500,"Не удалось прочитать материалы");return};if item.Kind=="question" {item.Status=questionStatusLabel(item.Status)} else {item.Status=documentStatusLabel(item.Status)};items=append(items,item)}
	if rows.Err()!=nil {c.String(500,"Не удалось прочитать материалы");return}
	c.HTML(200,"admin.html",gin.H{"Title":"Панель администратора","User":actor,"CSRFToken":app.templateCSRF(c),"Summary":s,"Materials":items,"Done":c.Query("done")})
}

func maintenanceAction(action string) (string,string,bool) {
	switch action {
	case "archive_all":return "Архивировать все материалы и отключить остальных пользователей","В АРХИВ",true
	case "reset":return "Полностью удалить данные и файлы, кроме текущего администратора","УДАЛИТЬ ВСЁ",true
	case "archive_question":return "Убрать вопрос в архив","В АРХИВ",true
	case "archive_document":return "Убрать документ в архив","В АРХИВ",true
	default:return "","",false
	}
}

func (app *application) maintenanceToken(actor user, action string, id int64, s adminSnapshot) string {
	return app.sessionDigest(fmt.Sprintf("maintenance:%d:%s:%d:%+v",actor.ID,action,id,s))
}

func (app *application) previewMaintenance(c *gin.Context) {
	actor:=c.MustGet("user").(user)
	action:=c.PostForm("action")
	title,phrase,ok:=maintenanceAction(action);if !ok {c.String(400,"Неизвестное действие");return}
	id,_:=strconv.ParseInt(c.PostForm("target_id"),10,64)
	var target string
	if action=="archive_question" || action=="archive_document" {
		table:="questions";if action=="archive_document" {table="documents"}
		if err:=app.db.QueryRow(c.Request.Context(),"SELECT title FROM "+table+" WHERE id=$1 AND archived_at IS NULL",id).Scan(&target);err!=nil {c.String(404,"Материал не найден или уже в архиве");return}
	}
	s,err:=loadAdminSnapshot(c.Request.Context(),app.db,actor.ID);if err!=nil {c.String(500,"Не удалось подготовить подтверждение");return}
	c.HTML(200,"maintenance.html",gin.H{"Title":title,"User":actor,"CSRFToken":app.templateCSRF(c),"Action":action,"TargetID":id,"Target":target,"Phrase":phrase,"Summary":s,"ConfirmationToken":app.maintenanceToken(actor,action,id,s)})
}

func (app *application) executeMaintenance(c *gin.Context) {
	actor:=c.MustGet("user").(user);ctx:=c.Request.Context()
	action:=c.PostForm("action");_,phrase,ok:=maintenanceAction(action)
	id,_:=strconv.ParseInt(c.PostForm("target_id"),10,64)
	reason:=strings.TrimSpace(c.PostForm("reason"))
	if !ok || c.PostForm("confirmation")!=phrase || reason=="" || len([]rune(reason))>2000 {c.String(422,"Укажите причину и точную подтверждающую фразу");return}
	// Reauthentication is limited independently of login attempts.
	key:="maintenance:"+strconv.FormatInt(actor.ID,10)
	if allowed,_:=app.loginLimiter.allow(key,time.Now());!allowed {c.String(429,"Слишком много неверных паролей. Повторите позднее");return}
	var hash string
	if err:=app.db.QueryRow(ctx,"SELECT password_hash FROM users WHERE id=$1 AND active=TRUE AND role='admin'",actor.ID).Scan(&hash);err!=nil || !verifyPassword(c.PostForm("password"),hash) {app.loginLimiter.failure(key,time.Now());c.String(403,"Пароль администратора неверен");return}
	app.loginLimiter.success(key)
	tx,err:=app.db.Begin(ctx);if err!=nil {c.String(500,"Не удалось начать обслуживание");return};defer tx.Rollback(ctx)
	// Fixed table list, no CASCADE and no sequence reset. Concurrent transactions
	// finish before the snapshot is checked; a stale preview cannot erase new work.
	if _,err=tx.Exec(ctx,"SET LOCAL lock_timeout='5s'");err==nil {_,err=tx.Exec(ctx,"LOCK TABLE users, sessions, "+resetTables+" IN ACCESS EXCLUSIVE MODE")}
	if err!=nil {c.String(409,"Система занята. Повторите предварительный просмотр после завершения операций");return}
	s,err:=loadAdminSnapshot(ctx,tx,actor.ID)
	if err!=nil {c.String(500,"Не удалось проверить состав данных");return}
	if !secureTokenEqual(c.PostForm("confirmation_token"),app.maintenanceToken(actor,action,id,s)) {c.String(409,"Данные изменились. Выполните предварительный просмотр заново");return}
	// Recheck the actor while the users table is locked.
	var active bool
	if err=tx.QueryRow(ctx,"SELECT active AND role='admin' AND password_hash=$2 FROM users WHERE id=$1",actor.ID,hash).Scan(&active);err!=nil || !active {c.String(403,"Доступ администратора изменился");return}
	if action=="reset" {
		if s.Files>0 && app.storage==nil {c.String(409,"Перед сбросом настройте S3 для удаления файлов");return}
		_,err=tx.Exec(ctx,`INSERT INTO storage_cleanup(endpoint,bucket,object_key,version_id)
			SELECT $1,$2,object_key,COALESCE(s3_version_id,'') FROM (
			SELECT object_key,s3_version_id FROM question_file_versions UNION ALL SELECT object_key,s3_version_id FROM document_versions
			UNION ALL SELECT object_key,s3_version_id FROM internal_visa_attachments UNION ALL SELECT object_key,s3_version_id FROM committee_vote_attachments) f
			ON CONFLICT DO NOTHING`,strings.TrimRight(os.Getenv("S3_ENDPOINT"),"/"),os.Getenv("S3_BUCKET"))
		if err==nil {_,err=tx.Exec(ctx,"TRUNCATE "+resetTables)}
		if err==nil {_,err=tx.Exec(ctx,"DELETE FROM sessions WHERE user_id<>$1",actor.ID)}
		if err==nil {_,err=tx.Exec(ctx,"DELETE FROM users WHERE id<>$1",actor.ID)}
	} else {
		if action=="archive_all" || action=="archive_question" {err=archiveQuestions(ctx,tx,actor,id,action=="archive_all",reason)}
		if err==nil && (action=="archive_all" || action=="archive_document") {err=archiveDocuments(ctx,tx,id,action=="archive_all")}
		if err==nil && action=="archive_all" {
			_,err=tx.Exec(ctx,"UPDATE users SET active=FALSE WHERE id<>$1",actor.ID)
			if err==nil {_,err=tx.Exec(ctx,"DELETE FROM sessions WHERE user_id<>$1",actor.ID)}
		}
	}
	if err==nil {err=app.writeAudit(ctx,tx,actor,auditRecord{EventType:"admin."+action,TargetType:"maintenance",TargetID:&id,Details:fmt.Sprintf("Вопросов: %d; документов: %d; других пользователей: %d; файлов: %d. Причина: %s",s.Questions,s.Documents,s.Users,s.Files,reason)})}
	if err==nil {err=tx.Commit(ctx)}
	if err!=nil {c.String(500,"Операция не выполнена. Изменения базы отменены");return}
	c.Redirect(303,"/admin?done="+action)
}

func archiveQuestions(ctx context.Context,tx pgx.Tx,actor user,id int64,all bool,reason string) error {
	for _,table:=range []string{"internal_review_rounds","committee_vote_rounds"} {
		if _,err:=tx.Exec(ctx,"UPDATE "+table+" SET status='cancelled',cancelled_at=NOW() WHERE status='active' AND question_id IN (SELECT id FROM questions WHERE archived_at IS NULL AND ($1 OR id=$2))",all,id);err!=nil{return err}
	}
	_,err:=tx.Exec(ctx,`UPDATE questions SET archived_at=NOW(),updated_at=NOW(),
		cancellation_reason=CASE WHEN status NOT IN ('approved','cancelled') THEN $3 ELSE cancellation_reason END,
		cancelled_by_name=CASE WHEN status NOT IN ('approved','cancelled') THEN $4 ELSE cancelled_by_name END,
		cancelled_at=CASE WHEN status NOT IN ('approved','cancelled') THEN NOW() ELSE cancelled_at END,
		status=CASE WHEN status='approved' THEN status ELSE 'cancelled' END WHERE archived_at IS NULL AND ($1 OR id=$2)`,all,id,reason,actor.FullName)
	return err
}
func archiveDocuments(ctx context.Context,tx pgx.Tx,id int64,all bool) error {
	if _,err:=tx.Exec(ctx,`UPDATE approval_rounds SET status='cancelled',completed_at=NOW() WHERE status='active' AND document_id IN (SELECT id FROM documents WHERE archived_at IS NULL AND ($1 OR id=$2))`,all,id);err!=nil{return err}
	_,err:=tx.Exec(ctx,`UPDATE documents SET archived_at=NOW(),status=CASE WHEN status='approved' THEN status ELSE 'rejected' END,updated_at=NOW() WHERE archived_at IS NULL AND ($1 OR id=$2)`,all,id);return err
}

func (app *application) processStorageCleanup(ctx context.Context) {
	if app.storage==nil{return}
	for i:=0;i<25;i++ {
		tx,err:=app.db.Begin(ctx);if err!=nil{return}
		var id int64;var key,version string
		err=tx.QueryRow(ctx,`SELECT id,object_key,version_id FROM storage_cleanup WHERE completed_at IS NULL AND endpoint=$1 AND bucket=$2 ORDER BY attempts,id LIMIT 1 FOR UPDATE SKIP LOCKED`,strings.TrimRight(os.Getenv("S3_ENDPOINT"),"/"),app.storage.bucket).Scan(&id,&key,&version)
		if err!=nil {tx.Rollback(ctx);return}
		input:=&s3.DeleteObjectInput{Bucket:&app.storage.bucket,Key:&key};if version!=""{input.VersionId=&version}
		callCtx,cancel:=context.WithTimeout(ctx,5*time.Second)
		_,deleteErr:=app.storage.client.DeleteObject(callCtx,input);cancel()
		_,err=tx.Exec(ctx,"UPDATE storage_cleanup SET attempts=attempts+1,completed_at=CASE WHEN $2 THEN NOW() ELSE NULL END WHERE id=$1",id,deleteErr==nil)
		if err==nil {err=tx.Commit(ctx)};tx.Rollback(ctx)
		if err!=nil || deleteErr!=nil{return}
	}
}
func (app *application) retryStorageCleanup(c *gin.Context) {app.processStorageCleanup(c.Request.Context());c.Redirect(303,"/admin")}
func (app *application) runStorageCleanup() {
	ticker:=time.NewTicker(time.Minute);defer ticker.Stop()
	for range ticker.C {ctx,cancel:=context.WithTimeout(context.Background(),30*time.Second);app.processStorageCleanup(ctx);cancel()}
}

func (app *application) restoreUser(c *gin.Context) {
	id,ok:=parsePositiveID(c,"id","Некорректный пользователь");if !ok{return}
	tx,err:=app.db.Begin(c.Request.Context());if err!=nil {c.String(500,"Не удалось начать восстановление");return};defer tx.Rollback(c.Request.Context())
	if err=lockUserAssignments(c,tx);err!=nil {c.String(500,"Не удалось проверить назначения");return}
	var role,name string;var chair bool
	err=tx.QueryRow(c.Request.Context(),"SELECT role,full_name,is_committee_chair FROM users WHERE id=$1 AND active=FALSE FOR UPDATE",id).Scan(&role,&name,&chair)
	if err!=nil {c.String(404,"Отключённый пользователь не найден");return}
	if err=ensureUniqueActiveAssignment(c,tx,role,chair,id);err!=nil {c.String(409,"Это назначение уже занято. Сначала освободите роль секретаря или председателя");return}
	_,err=tx.Exec(c.Request.Context(),"UPDATE users SET active=TRUE,must_change_password=TRUE WHERE id=$1",id)
	if err==nil {err=app.writeAudit(c.Request.Context(),tx,c.MustGet("user").(user),auditRecord{EventType:"user.restored",TargetType:"user",TargetID:&id,TargetLabel:name})}
	if err==nil {err=tx.Commit(c.Request.Context())}
	if err!=nil {c.String(500,"Не удалось восстановить доступ");return};c.Redirect(303,"/admin/users")
}
func (app *application) revokeUserSessions(c *gin.Context) {
	id,ok:=parsePositiveID(c,"id","Некорректный пользователь");if !ok{return}
	tx,err:=app.db.Begin(c.Request.Context());if err!=nil {c.String(500,"Не удалось завершить сессии");return};defer tx.Rollback(c.Request.Context())
	var name string
	if err=tx.QueryRow(c.Request.Context(),"SELECT full_name FROM users WHERE id=$1",id).Scan(&name);err!=nil {c.String(404,"Пользователь не найден");return}
	_,err=tx.Exec(c.Request.Context(),"DELETE FROM sessions WHERE user_id=$1",id)
	if err==nil {err=app.writeAudit(c.Request.Context(),tx,c.MustGet("user").(user),auditRecord{EventType:"user.sessions_revoked",TargetType:"user",TargetID:&id,TargetLabel:name})}
	if err==nil {err=tx.Commit(c.Request.Context())};if err!=nil {c.String(500,"Не удалось завершить сессии");return};c.Redirect(303,"/admin/users")
}
