package main

func canManageQuestions(usr user) bool {
	return usr.Role == "admin" || usr.Role == "secretary"
}
