const { LavaSDK } = require("../../../../ecosystem/lava-sdk/bin/src/sdk/sdk");

function delay(ms: number) {
    return new Promise( resolve => setTimeout(resolve, ms) );
}

async function main() {
    // Initialize Lava SDK
    const lavaSDKTendermint = await LavaSDK.create({
        privateKey: process.env.PRIVATE_KEY,
        chainIds: "LAV1",
        lavaChainId: "lava",
        pairingListConfig: process.env.PAIRING_LIST,
        allowInsecureTransport: true,
        logLevel: "debug",
    }).catch((e) => {
        throw new Error(" ERR [tendermintrpc_chainid_fetch] failed setting lava-sdk tendermint test");
    });

    // Fetch chain id
    for (let i = 0; i < 26; i++) { // send relays synchronously
        try {
            const result = await lavaSDKTendermint.sendRelay({
                method: "status",
                params: [],
            });

            // Parse response
            const parsedResponse = result;

            const chainID = parsedResponse.result["node_info"].network;

            // Validate chainID
            if (chainID !== "lava") {
                throw new Error(" ERR [tendermintrpc_chainid_fetch] Chain ID is not equal to lava");
            } else {
                console.log(i, "[tendermintrpc_chainid_fetch] Success: Fetching Lava chain ID using tendermintrpc passed. Chain ID correctly matches 'lava'");
            }
        } catch (error) {
            throw new Error(` ERR ${i} [tendermintrpc_chainid_fetch] failed sending relay tendermint test: ${error.message}`);
        }
    }
}

(async () => {
    try {
        await main();
        process.exit(0);
    } catch (error) {
        //console.error(" ERR [tendermintrpc_chainid_fetch] " + error.message);
        process.exit(1);
    }
})();