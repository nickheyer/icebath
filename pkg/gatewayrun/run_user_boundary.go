package gatewayrun

// End reasons of a session the user closed, a boundary no late completion may cross
var userBoundaryEndReasons = map[string]bool{"session_reset": true, "user_exit": true, "session_switch": true, "new_session": true}
