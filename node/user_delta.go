package node

import (
	"sort"

	panel "github.com/wyx2685/v2node/api/v2board"
)

func applyUserDeltaEvents(oldUsers []panel.UserInfo, events []panel.UserDeltaEvent) ([]panel.UserInfo, bool) {
	if len(events) == 0 {
		return oldUsers, false
	}

	usersByUUID := make(map[string]panel.UserInfo, len(oldUsers))
	for _, user := range oldUsers {
		if user.Uuid == "" {
			continue
		}
		usersByUUID[user.Uuid] = user
	}

	changed := false
	for _, event := range events {
		userID := event.UserID
		if userID <= 0 && len(event.Users) > 0 {
			userID = event.Users[0].Id
		}

		switch event.Action {
		case panel.UserDeltaActionUpsert:
			if userID > 0 {
				deleteUserID(usersByUUID, userID)
			}
			for _, user := range event.Users {
				if user.Uuid == "" {
					continue
				}
				usersByUUID[user.Uuid] = user
			}
			changed = true
		case panel.UserDeltaActionDelete:
			if userID > 0 {
				deleteUserID(usersByUUID, userID)
				changed = true
				continue
			}
			for _, user := range event.Users {
				if user.Uuid == "" {
					continue
				}
				delete(usersByUUID, user.Uuid)
				changed = true
			}
		}
	}

	if !changed {
		return oldUsers, false
	}

	next := make([]panel.UserInfo, 0, len(usersByUUID))
	for _, user := range usersByUUID {
		next = append(next, user)
	}
	sort.Slice(next, func(i, j int) bool {
		if next[i].Id != next[j].Id {
			return next[i].Id < next[j].Id
		}
		return next[i].Uuid < next[j].Uuid
	})
	return next, true
}

func deleteUserID(usersByUUID map[string]panel.UserInfo, userID int) {
	for uuid, user := range usersByUUID {
		if user.Id == userID {
			delete(usersByUUID, uuid)
		}
	}
}

func removeExpiredUsers(users []panel.UserInfo, nowUnix int64) ([]panel.UserInfo, bool) {
	if len(users) == 0 {
		return users, false
	}

	next := make([]panel.UserInfo, 0, len(users))
	changed := false
	for _, user := range users {
		if user.ExpiredAt > 0 && user.ExpiredAt <= nowUnix {
			changed = true
			continue
		}
		next = append(next, user)
	}
	if !changed {
		return users, false
	}
	return next, true
}
