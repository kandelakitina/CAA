# Neva Concert Hall Corporate Approvals

Go/Gin/HTMX-приложение с PostgreSQL, закрытым входом и приватным S3 для Timeweb Cloud App Platform.

Текущая версия позволяет администратору или секретарю создать карточку документа,
загружать последовательные версии `.doc`, `.docx` или `.pdf` размером до 25 МБ,
просматривать историю и скачивать любую редакцию. Метаданные и номера версий
хранятся в PostgreSQL, файлы — в S3.

Администратор также может создавать пользователей с ролями секретаря, члена
комитета, согласующего и наблюдателя. Новый пользователь обязан сменить временный
пароль при первом входе.

## Локальный запуск

```bash
go mod download
go run .
```

Откройте `http://localhost:8080`. Проверка состояния доступна по адресу `/health`.

Для запуска нужны переменные `PGHOST`, `PGPORT`, `PGDATABASE`, `PGUSER`, `PGPASSWORD`,
`PGSSLMODE`, `SESSION_SECRET`, `INITIAL_ADMIN_EMAIL`, `INITIAL_ADMIN_PASSWORD` и
опционально `INITIAL_ADMIN_NAME`.

Для S3 также нужны `S3_ENDPOINT`, `S3_REGION`, `S3_BUCKET`, `S3_ACCESS_KEY_ID` и
`S3_SECRET_ACCESS_KEY`.

При первом запуске приложение создаст таблицы `users` и `sessions`, а затем первого
администратора. После успешного входа `INITIAL_ADMIN_PASSWORD` следует удалить из
переменных App Platform: хэш пароля уже сохранен в PostgreSQL.

## Настройки Timeweb

- Тип: Backend → Gin
- Команда сборки: `go build -o app .`
- Команда запуска: `./app`
- Путь проверки состояния: `/health`
- Путь до директории проекта: пустой, если эти файлы находятся в корне репозитория
