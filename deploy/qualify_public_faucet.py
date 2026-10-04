#!/usr/bin/env python3
"""Make one fenced Testnet payout after independent NU7 and website qualification."""
import argparse
import decimal
import hashlib
import json
import os
from pathlib import Path
import re
import time
import tomllib
import urllib.request

ACTIVATION = 4465026
BRANCH = '77190ad9'
RECIPIENT = 'tmDCiNGTbRz1Y1eYWyrPBFCaH61JSffwzSr'
AMOUNT_ZAT = 12500000
API = 'https://faucet.testnet.valargroup.dev/api'


def require(condition):
    if not condition:
        raise ValueError("Qualification gate failed")


def fetch(url, body=None):
    request = urllib.request.Request(url, None if body is None else json.dumps(body).encode(),
        {'Content-Type': 'application/json', 'Origin': 'https://zakura.com'})
    with urllib.request.urlopen(request, timeout=12) as response:
        content = response.read(2097153)
        if len(content) > 2097152:
            raise ValueError('HTTP response exceeds bounded size')
        return json.loads(content)


def rpc(url, method, params):
    response = fetch(url, {'jsonrpc': '2.0', 'id': 1, 'method': method, 'params': params})
    if response.get('error'):
        raise ValueError('RPC did not return a successful result')
    return response['result']


def persist(path, value, exclusive=False):
    temporary = path if exclusive else path.with_suffix('.tmp')
    with temporary.open('x' if exclusive else 'w') as file:
        file.write(json.dumps(value, indent=2) + '\n')
        file.flush()
        os.fsync(file.fileno())
    if not exclusive:
        temporary.replace(path)
    descriptor = os.open(path.parent, os.O_RDONLY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def validate_envelope(envelope):
    require(envelope['selectedProfile'] == 'public-testnet')
    require(envelope['selectionState'] == 'selected')
    require(0 <= time.time() - envelope['generatedAt'] <= 90)
    status = envelope['status']
    require(status['status'] == 'live' and status['network']['magic'] == 'fa1af9bf')
    require(status['observation']['validatorsAgree'])
    require(status['chain']['height'] >= ACTIVATION + 2)
    body = {key: value for key, value in envelope.items() if key != 'generation'}
    require(hashlib.sha256(json.dumps(body, sort_keys=True, allow_nan=False).encode()).hexdigest()
        == envelope['generation'])
    manifest = envelope['network']
    network = manifest['network']
    require(network['name'] == 'Testnet' and network['magic'] == 'fa1af9bf')
    require(network['activationHeight'] == ACTIVATION and network['branchId'] == BRANCH)
    require(all(status['network'].get(key) == network[key]
        for key in ('name', 'magic', 'activationHeight', 'branchId', 'targetSpacingSeconds', 'daaWindowBlocks')))
    revision = manifest['nodeRevision']
    require(re.fullmatch('[0-9a-f]{40}', revision) is not None)
    require(hashlib.sha256(manifest['config'].encode()).hexdigest() == manifest['configSha256'])
    joining = tomllib.loads(manifest['config'])
    require(set(joining) <= {'network', 'rpc', 'state'})
    require(joining['network']['network'] == 'Testnet')
    require(set(joining['network']) <= {'network', 'listen_addr', 'initial_testnet_peers', 'p2p_stack'})
    for key, height in [('atTip', status['chain']['height']), ('nextBlock', status['chain']['height'] + 1)]:
        rules = envelope['rules'][key]
        require(rules['effectiveHeight'] == height and rules['network'] == 'Testnet')
        require(rules['networkMagic'] == 'fa1af9bf' and rules['activationHeight'] == ACTIVATION)
        require(rules['branchId'] == BRANCH and revision[:12] in rules['buildVersion'])
        require(rules['targetSpacingSeconds'] == network['targetSpacingSeconds'])
        require(rules['difficulty']['averagingWindowBlocks'] == network['daaWindowBlocks'])
    capability = envelope['capabilities']['faucet']
    require(capability['apiUrl'] == API and capability['claimZat'] == AMOUNT_ZAT)
    # Public snapshots remain disabled until a separately qualified artifact exists.
    require(manifest.get('snapshot') is None and envelope['capabilities']['snapshot'] is None)


def gate(config):
    envelope = fetch(config['selectorUrl'])
    validate_envelope(envelope)
    local = rpc(config['nodeRpc'], 'getblockchaininfo', [])
    require(local['chain'] == 'test' and local['blocks'] >= ACTIVATION + 2)
    parameters = rpc(config['nodeRpc'], 'getnetworkparameters', [local['blocks']])
    require(parameters['networkMagic'] == 'fa1af9bf')
    require(parameters['activationHeight'] == ACTIVATION and parameters['branchId'] == BRANCH)
    require(envelope['network']['nodeRevision'][:12] in parameters['buildVersion'])
    require(parameters['difficulty'] == envelope['rules']['atTip']['difficulty'])
    require(parameters['difficulty'] == envelope['rules']['nextBlock']['difficulty'])
    independent = rpc(config['referenceRpc'], 'getblockchaininfo', [])
    software = rpc(config['referenceRpc'], 'getnetworkinfo', [])
    require(re.match(r'^/Zebra:7\.', software['subversion']))
    require(independent['chain'] == 'test' and independent['blocks'] >= ACTIVATION + 2)
    require(independent['consensus']['chaintip'].lower().removeprefix('0x') == BRANCH)
    checkpoint = rpc(config['referenceRpc'], 'getblockhash', [ACTIVATION + 2])
    require(rpc(config['nodeRpc'], 'getblockhash', [ACTIVATION + 2]) == checkpoint)
    faucet = fetch(API + '/status')
    require(faucet['ready'] and faucet['network'] == 'testnet')
    require(decimal.Decimal(faucet['payout']) * 100000000 == AMOUNT_ZAT)
    return {'observedAt': int(time.time()), 'nodeHeight': local['blocks'],
        'referenceHeight': independent['blocks'], 'checkpointHash': checkpoint,
        'selectorGeneration': envelope['generation'], 'branchId': BRANCH}


def qualify(config):
    receipt = Path(config['receipt'])
    state = json.loads(receipt.read_text()) if receipt.exists() else None
    if state and state.get('accepted'):
        return 'complete'  # No network calls after acceptance.
    if state and not state.get('claimId'):
        return 'ambiguous'  # A durable attempt without an ID must never POST again.
    if state is None:
        evidence = gate(config)
        if not config.get('enabled', False):
            return 'qualified-read-only'
        receipt.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        state = {'request': {'address': RECIPIENT}, 'amountZat': AMOUNT_ZAT,
            'gate': evidence, 'attemptStartedAt': int(time.time())}
        persist(receipt, state, exclusive=True)
        response = fetch(API + '/claim', state['request'])
        require(re.fullmatch('[0-9a-f]{32}', response['id']))
        require(decimal.Decimal(response['amount']) * 100000000 == AMOUNT_ZAT)
        state.update(claimId=response['id'], response=response)
        persist(receipt, state)
    claim = fetch(API + '/claim/' + state['claimId'])
    state['claim'] = claim
    persist(receipt, state)
    if claim['status'] in ('failed', 'review'):
        return 'review'
    if claim['status'] != 'sent':
        return 'pending'
    require(re.fullmatch('[0-9a-f]{64}', claim['txid']))
    transaction = rpc(config['nodeRpc'], 'getrawtransaction', [claim['txid'], 1])
    raw = bytes.fromhex(transaction['hex'])
    require(transaction['version'] == 6 and raw[8:12][::-1].hex() == BRANCH)
    if transaction.get('confirmations', 0) < 2:
        return 'pending'
    require(transaction['height'] >= ACTIVATION)
    require(any(RECIPIENT in output['scriptPubKey'].get('addresses', [])
        and decimal.Decimal(str(output['value'])) * 100000000 == AMOUNT_ZAT
        for output in transaction['vout']))
    require(rpc(config['referenceRpc'], 'getblockhash', [transaction['height']]) == transaction['blockhash'])
    state['accepted'] = {key: transaction[key] for key in ('txid', 'height', 'blockhash', 'confirmations')}
    state['accepted'].update(version=6, branchId=BRANCH, recipient=RECIPIENT, amountZat=AMOUNT_ZAT,
        observedAt=int(time.time()))
    persist(receipt, state)
    return 'complete'


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--config', type=Path, required=True)
    args = parser.parse_args()
    config = json.loads(args.config.read_text())
    try:
        result = qualify(config)
    except (AssertionError, KeyError, ValueError, OSError) as error:
        print('Qualification pending or requires investigation: ' + type(error).__name__)
        # Missing activation/selection is normal before the single attempt.
        return 1 if Path(config['receipt']).exists() else 0
    print('Public faucet qualification: ' + result)
    return 2 if result in ('ambiguous', 'review') else 0


if __name__ == '__main__':
    raise SystemExit(main())
