"""Owner-specific presentation hints; never rewrite resource or sender IDs."""

def project_message(message, viewer_ids):
    if not isinstance(message, dict):
        return 0
    count = 0
    if ('raw_messages' not in message
            and message.get('role', 'user') == 'user'
            and isinstance(message.get('account_user_id'), str)
            and message['account_user_id'] in viewer_ids):
        message['is_self'] = True
        count += 1
    reply = message.get('reply_to')
    if isinstance(reply, dict):
        count += project_message(reply.get('message'), viewer_ids)
    return count

def project_envelope(envelope, viewer_ids):
    count = 0
    for key in ['items', 'latest_messages']:
        messages = envelope.get(key, [])
        if isinstance(messages, list):
            count += sum(project_message(message, viewer_ids) for message in messages)
    return count


def project_response(value, viewer_ids, direct=False):
    if direct:
        return project_message(value, viewer_ids)
    if not isinstance(value, dict):
        return 0
    return project_envelope(value, viewer_ids)
