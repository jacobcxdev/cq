const assert = require('node:assert/strict');
const sources = JSON.parse(require('node:fs').readFileSync(0, 'utf8'));

(async () => {
    for (const [kind, source] of Object.entries(sources)) {
        for (const invalid of [null, 'profile', 'root', 'room', 'empty-root', 'empty-room', ...(kind === 'send' ? ['principal', 'selection'] : [])]) {
            let writes = 0;
            const fakeApi = {
                async safeGet(path, options) {
                    if (path === '/tbo/primary') {
                        return {
                            profile: {id: invalid === 'principal' ? 'foreign' : 'profile-one', active_root_thread_id: 'rotated-primary-root'},
                            selection: {thread_id: invalid === 'selection' ? 'foreign-root' : 'rotated-primary-root'},
                        };
                    }
                    if (path === '/tbo/{tbo_id}') {
                        assert.equal(options.parameters.path.tbo_id, 'profile-two');
                        return {
                            id: invalid === 'profile' ? 'foreign' : 'profile-two',
                            active_root_thread_id: invalid === 'root' ? null : invalid === 'empty-root' ? '' : 'rotated-secondary-root',
                            messaging_room_id: invalid === 'room' ? null : invalid === 'empty-room' ? '' : 'owner-room',
                        };
                    }
                    if (path === '/ios/attestation_challenge') return {attestation_challenge: 'native-proof'};
                    assert.equal(options.parameters.path.room_id, 'owner-room');
                    if (path === '/messaging/rooms/{room_id}') return {members: [{aeon_id: 'profile-two', account_user_id: 'owner-dot-actor'}]};
                    assert.equal(path, '/messaging/rooms/{room_id}/messages');
                    return {items: [{id: 'reply-id', account_user_id: 'owner-dot-actor', content: {text: '[CQ:directory:0123456789abcdef0123456789abcdef]\ndirectory\n[/CQ]'}}]};
                },
                async safePost(path, options) {
                    assert.equal(path, '/messaging/rooms/{room_id}/messages');
                    assert.equal(options.parameters.path.room_id, 'owner-room');
                    assert.equal(options.requestBody.app_attest_challenge, 'native-proof');
                    assert.equal(options.retry, false);
                    writes++;
                    return {id: 'sent-id', content: options.requestBody.content};
                },
            };
            const run = () => eval(source.replace(/import\([^)]*\)/, 'Promise.resolve({Ymr:fakeApi,iRr:fakeApi,_lr:fakeApi})'));
            if (invalid) {
                await assert.rejects(run, /Target changed|Native principal changed/);
                assert.equal(writes, 0);
            } else {
                const result = await run();
                if (kind === 'send') {
                    assert.equal(result.accepted, true);
                    assert.equal(writes, 1);
                } else if (kind === 'relay') {
                    assert.equal(result.length, 1);
                } else {
                    assert.equal(result.match.length, 1);
                }
            }
        }
    }
})().catch(error => { console.error(error); process.exitCode = 1; });
