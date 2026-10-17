import test_support
import json, unittest
import probe

class ChallengeTests(unittest.TestCase):
    def setUp(self):
        probe.CHALLENGES.clear()
        self.primary = json.dumps({'attestation_challenge':'primary-proof','expire_after':10})
        self.worker = json.dumps({'attestation_challenge':'owner-proof','expire_after':5})
        self.body = json.dumps({'app_attest_challenge':'primary-proof','content':{'text':'example'},
                               'request_id':'id','idempotency_token':'id','reply_to':{'message_id':'reply'}})

    def test_only_challenge_changes_and_consumes_once(self):
        probe.remember_challenge(self.primary,self.worker,'worker',now=1)
        result=json.loads(probe.owner_message_body(self.body,'worker',now=2))
        expected=json.loads(self.body)
        expected['app_attest_challenge']='owner-proof'
        self.assertEqual(result,expected)
        with self.assertRaises(ValueError): probe.owner_message_body(self.body,'worker',now=2)

    def test_wrong_owner_cannot_consume_and_expiry_rejects(self):
        probe.remember_challenge(self.primary,self.worker,'worker',now=1)
        with self.assertRaises(ValueError): probe.owner_message_body(self.body,'other',now=2)
        with self.assertRaises(ValueError): probe.owner_message_body(self.body,'worker',now=6)

    def test_no_missing_or_malformed_proof_fallback(self):
        for bad in ['{}','[]',json.dumps({'attestation_challenge':''}),
                    json.dumps({'attestation_challenge':'proof','expire_after':False})]:
            with self.assertRaises((ValueError,AttributeError)): probe.remember_challenge(bad,self.worker,'worker',now=1)
        with self.assertRaises(ValueError): probe.owner_message_body(self.body,'worker',now=2)

    def test_owner_headers_preserve_native_device_proof(self):
        headers={'Authorization':'main','ChatGPT-Account-Id':'main','Cookie':'main-cookie',
                 'X-OpenAI-Expected-Account-Id':'main','X-OpenAI-Orbit-Selection':'main',
                 'x-sentinel-dc':'native-proof','OAI-Package-Name':'native-package','oai-did':'native-device'}
        result=probe.owner_headers(headers,{'token':'worker-token','account_id':'worker'})
        self.assertEqual(result['Authorization'],'Bearer worker-token')
        self.assertEqual(result['ChatGPT-Account-Id'],'worker')
        self.assertNotIn('Cookie',result)
        self.assertNotIn('X-OpenAI-Expected-Account-Id',result)
        for name in ['x-sentinel-dc','OAI-Package-Name','oai-did']:
            self.assertEqual(result[name],headers[name])

if __name__ == '__main__': unittest.main()
