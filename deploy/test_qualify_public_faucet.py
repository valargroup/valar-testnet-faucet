import json
import hashlib
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
        network = {'name':'Testnet', 'magic':'fa1af9bf', 'activationHeight':subject.ACTIVATION,
            'branchId':subject.BRANCH, 'targetSpacingSeconds':25, 'daaWindowBlocks':102}
        config = '[network]\nnetwork = "Testnet"\n'
        rules = {'network':'Testnet', 'networkMagic':'fa1af9bf', 'activationHeight':subject.ACTIVATION,
            'branchId':subject.BRANCH, 'targetSpacingSeconds':25, 'buildVersion':'zakurad +g'+'a'*12,
            'difficulty': {'averagingWindowBlocks':102}}
        envelope = {'selectedProfile': 'public-testnet', 'selectionState': 'selected',
            'generatedAt': time.time(), 'generation': 'generation', 'status': {'status': 'live',
            'network': network.copy(), 'observation': {'validatorsAgree': True},
            'chain': {'height': subject.ACTIVATION + 2}}, 'network':{'network':network,
                'nodeRevision':'a'*40, 'config':config, 'configSha256':hashlib.sha256(config.encode()).hexdigest()},
            'rules': {'atTip':{**rules, 'effectiveHeight':subject.ACTIVATION+2},
                'nextBlock':{**rules, 'effectiveHeight':subject.ACTIVATION+3}},
            'capabilities':{'faucet':{'apiUrl':subject.API, 'claimZat':subject.AMOUNT_ZAT}}}
        self.digest(envelope)
        return envelope

    def digest(self, envelope):
        envelope['generation'] = hashlib.sha256(json.dumps({key:value for key,value in envelope.items()
            if key != 'generation'},sort_keys=True,allow_nan=False).encode()).hexdigest()

    def test_manifest_config_rules_and_capability_must_be_coherent(self):
        mutations = [lambda value: value['network']['network'].update(branchId='37a5165b'),
            lambda value: value['network'].update(config='[network.network]\nnetwork_name="Custom"\n'),
            lambda value: value['rules']['nextBlock'].update(effectiveHeight=subject.ACTIVATION+2),
            lambda value: value['capabilities']['faucet'].update(apiUrl='https://example.invalid'),
            lambda value: value['network'].update(configSha256='0'*64)]
        for mutation in mutations:
            value = self.envelope(); mutation(value); self.digest(value)
            with self.assertRaises(ValueError):
                subject.validate_envelope(value)

    def test_valid_selected_manifest_passes_and_tampered_generation_fails(self):
        value = self.envelope(); subject.validate_envelope(value)
        value['generation'] = '0'*64
        with self.assertRaises(ValueError):
            subject.validate_envelope(value)

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
                    return self.envelope()['rules']['atTip']
                if method == 'getnetworkinfo':
                    return {'subversion': version}
                self.fail('Reference rejection should precede payout')
            with patch.object(subject, 'fetch', return_value=self.envelope()), patch.object(subject, 'rpc', side_effect=rpc):
                with self.assertRaises(ValueError):
                    subject.gate(self.config)


if __name__ == '__main__':
    unittest.main()
