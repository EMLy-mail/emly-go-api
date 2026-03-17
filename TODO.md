## TODO: Backport the following routes:

Bug Report:
- [x] `GET /api/v1/bug-reports` - List all bug reports
- [x] `GET /api/v1/bug-reports/count` - Get the count of all bug reports
- [x] `GET /api/v1/bug-reports/{id}` - Get a specific bug report
- [x] `PATCH /api/v1/bug-reports/{id}/status` - Update the status of a specific bug report
- [x] `GET /api/v1/bug-reports/{id}/files/{file_id}` - Download a specific file attached to a bug report
- [x] `GET /api/v1/bug-reports/{id}/download` - Download all files attached to a specific bug report as a ZIP archive
- [ ] `DELETE /api/v1/bug-reports/{id}` - Delete a specific bug report
- [x] `POST /api/v1/bug-reports` - Create a new bug report with optional file attachments