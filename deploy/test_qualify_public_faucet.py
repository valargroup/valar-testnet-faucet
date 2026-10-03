import json
from pathlib import Path
import tempfile
import time
import unittest
from unittest.mock import patch

import qualify_public_faucet as subject


class QualificationTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.receipt = Path(self.directory.name) / 'receipt.json'
        self.config = {'receipt': str(self.receipt), 'enabled': True,
            'nodeRpc': 'node', 'referenceRpc': 'reference', 'selectorUrl': 'selector'}

    def test_complete_receipt_never_contacts_network(self):
        self.receipt.write_text(json.dumps({'accepted': {'txid': 'a' * 64}}))
        with patch.object(subject, 'fetch', side_effect=AssertionError('network call')):
            self.assertEqual(subject.qualify(self.config), 'complete')

    def test_ambiguous_post_is_fenced_before_transport_and_never_retried(self):
        def fail_post(url, body=None):
            self.assertTrue(self.receipt.exists())
            self.assertEqual(json.loads(self.receipt.read_text())['request']['address'], subject.RECIPIENT)
            raise TimeoutError('ambiguous transport')
        with patch.object(subject, 'gate', return_value={}), patch.object(subject, 'fetch', side_effect=fail_post) as fetch:
            with self.assertRaises(TimeoutError):
                subject.qualify(self.config)
            self.assertEqual(subject.qualify(self.config), 'ambiguous')
            self.assertEqual(fetch.call_count, 1)

    def test_pending_claim_resumes_without_second_post(self):
        self.receipt.write_text(json.dumps({'claimId': 'a' * 32}))
        with patch.object(subject, 'gate', side_effect=AssertionError('re-gate')), patch.object(subject, 'fetch', return_value={'status': 'queued'}) as fetch:
            self.assertEqual(subject.qualify(self.config), 'pending')
            fetch.assert_called_once_with(subject.API + '/claim/' + 'a' * 32)

    def test_disabled_qualification_has_no_attempt_receipt(self):
        self.config['enabled'] = False
        with patch.object(subject, 'gate', return_value={}), patch.object(subject, 'fetch') as fetch:
            self.assertEqual(subject.qualify(self.config), 'qualified-read-only')
            fetch.assert_not_called()
            self.assertFalse(self.receipt.exists())

    def envelope(self):
        return {'selectedProfile': 'public-testnet', 'selectionState': 'selected',
            'generatedAt': time.time(), 'generation': 'generation', 'status': {'status': 'live',
            'network': {'magic': 'fa1af9bf'}, 'observation': {'validatorsAgree': True},
            'chain': {'height': subject.ACTIVATION + 2}}}

    def test_staging_or_stale_selector_cannot_start_payout(self):
        for profile, age in [('staging', 0), ('public-testnet', 91)]:
            envelope = self.envelope(); envelope['selectedProfile'] = profile; envelope['generatedAt'] -= age
            with patch.object(subject, 'fetch', return_value=envelope), patch.object(subject, 'rpc') as rpc:
                with self.assertRaises(ValueError):
                    subject.gate(self.config)
                rpc.assert_not_called()

    def test_old_or_wrong_branch_reference_cannot_start_payout(self):
        for version, branch in [('/Zebra:6.3.0/', subject.BRANCH), ('/Zebra:7.0.0-rc.0/', '37a5165b')]:
            def rpc(url, method, params):
                if method == 'getblockchaininfo':
                    return {'chain': 'test', 'blocks': subject.ACTIVATION + 2, 'consensus': {'chaintip': branch}}
                if method == 'getnetworkparameters':
                    return {'networkMagic': 'fa1af9bf', 'activationHeight': subject.ACTIVATION, 'branchId': subject.BRANCH}
                if method == 'getnetworkinfo':
                    return {'subversion': version}
                self.fail('Reference rejection should precede payout')
            with patch.object(subject, 'fetch', return_value=self.envelope()), patch.object(subject, 'rpc', side_effect=rpc):
                with self.assertRaises(ValueError):
                    subject.gate(self.config)


if __name__ == '__main__':
    unittest.main()
