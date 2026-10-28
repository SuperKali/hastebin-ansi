package handler

// CreateDocumentAs is CreateDocumentFrom for any uploader: the paste is held
// to the limits of u and charged to its quota, then u.Commit runs, when set.
// When the commit fails the content is deleted again, the quota given back,
// and the commit error returned. MCP uploads of registered users use it.
func (h *DocumentHandler) CreateDocumentAs(u Uploader, content string) (string, error) {
	key, err := h.storeContent(u, "", content)
	if err != nil || u.Commit == nil {
		return key, err
	}
	size := int64(len(content))
	if err := u.Commit(key, size); err != nil {
		h.discard(u, key, size)
		return "", err
	}
	return key, nil
}
