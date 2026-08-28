# Two Prices for the Same Model: Building Claude Burst

I recently ran a comparison out of curiosity: what would a normal month of Claude Code usage on a fixed subscription have cost if it had instead been billed at public API rates. I built some local tooling to reconstruct that from session usage, because the standard analytics were not giving me the visibility I wanted into what individual sessions were actually consuming.

The result was startling. A heavy user on a flat monthly subscription had generated a token volume that, translated into public API pricing, was worth a large multiple of what the subscription actually costs.

That looked impossible at first. Either my measurement was badly wrong, or Anthropic was selling the same intelligence through two commercial models whose economics were radically different.

The answer turns out to be more interesting than either explanation, and building something around that answer turned out to be a longer job than the idea itself suggested.

## 1. Claude does not really have one price

When people say that Claude costs a certain amount, they are usually mixing together several very different products. Anthropic sells access to the same broad model family through subscriptions, through its metered API, through enterprise products, and through cloud providers such as Amazon Bedrock. The model may be recognisably the same, but the commercial contract around each route is not.

Claude Max is a fixed monthly subscription that includes Claude Code. It is not unlimited: Anthropic applies rolling session limits that reset every five hours, weekly limits, and potentially other capacity controls. By contrast, API access is explicitly metered, so the more tokens you consume, the more you owe.

Amazon Bedrock also provides metered access to Anthropic models, but the bill arrives through AWS rather than directly from Anthropic.

| Route | Commercial model | What stops you | Typical identity |
| --- | --- | --- | --- |
| Claude Max | Fixed monthly subscription | Five hour, weekly and other usage limits | Individual Claude login |
| Anthropic API | Metered token consumption | Rate limits, spend limits or your budget | API or platform credential |
| Amazon Bedrock | Metered cloud consumption | AWS quotas, policy or your budget | AWS credential or Bedrock API key |

This means that asking what Claude "costs" is already the wrong question. A better question is what Claude costs through a particular commercial channel for a particular workload shape.

## 2. Does a large API equivalent value mean Anthropic is losing money on that account?

No. This is the first distinction that matters.

An API equivalent cost estimate answers a hypothetical question: what would this observed token mix have cost if it had been charged at public API list prices? It does not tell us Anthropic's marginal cost of inference, its internal cost of compute, the effect of capacity commitments, its gross margin, or the economics of serving a subscription user during otherwise underutilised capacity.

The quality of the estimate also depends heavily on the measurement. Claude Code makes extensive use of prompt caching, so a tool that simply multiplies every input token by the normal input token price can significantly overstate the equivalent API bill. A credible calculator needs to distinguish uncached input, cache writes, cache reads, output tokens, the model used, and any other pricing buckets that matter at the time.

My point is therefore not that Anthropic is necessarily losing money on any particular subscriber. The point is that the public value of the consumption, when translated into another Anthropic pricing model, can be dramatically larger than the subscription fee. That pricing discontinuity is real even if Anthropic's internal cost is far below the API equivalent number.

## 3. Why would Anthropic price it this way?

The most useful way to think about Max is not as cheap API access. It is closer to a bounded block of interactive capacity sold to one human.

Anthropic controls the risk through time windows, weekly allowances, model limits and other usage constraints. A human developer also consumes capacity differently from a production API workload that can fan out thousands of requests, run continuously, serve external customers and create much less predictable concurrency. The API customer is buying something different even if the underlying model weights are the same.

There is also straightforward market segmentation. A fixed subscription gives an individual a psychologically simple price and encourages deep adoption, while API pricing captures value from workloads that scale with software rather than with one person's working day. Enterprise products add administration, contractual controls, governance and support on top of the model access itself.

This is common in computing. The interesting part is the size of the gap that appears once coding agents become heavy consumers of tokens, because a single strong engineer can now generate an amount of inference that would once have looked like a small application workload.

## 4. The obvious architecture is Max first, metered overflow second

Once I understood the distinction, the architecture became almost embarrassingly obvious.

Suppose an engineer has their own Max subscription and uses Claude Code interactively. While that subscription has capacity, use it. When Anthropic itself says that the five hour or weekly allowance has been exhausted, stop sending that engineer's inference through the subscription and route the overflow to the same Claude model through Amazon Bedrock.

When the subscription window resets, move the engineer back automatically.

The cost equation becomes very simple:

**effective monthly cost = Max subscription + actual metered overflow**

You are no longer choosing between Claude Max and Bedrock. You are using them as two capacity tiers for the same underlying model family. Max becomes the included base capacity and Bedrock becomes the burst layer.

This is conceptually similar to many infrastructure patterns we have used for years. You consume the cheaper bounded capacity first, then pay an on demand rate only for the demand that exceeds it.

## 5. Anthropic already exposes the pieces required to build this

I initially assumed that implementing the idea would require an ugly wrapper around Claude Code. It does not.

Anthropic's current Claude Code documentation explicitly supports an LLM gateway through the `ANTHROPIC_BASE_URL` configuration. More importantly, the documentation says that if you set only the base URL and do not provide a replacement gateway credential, Claude Code keeps using the developer's saved claude.ai subscription login. Requests pass through the gateway, but the normal subscription usage limits and billing still apply.

That gives us exactly the interception point we need. Claude Code also exposes the five hour and seven day subscription windows together with their reset timestamps, and distinguishes account usage limits from temporary server throttling. That means a router does not need to guess when somebody is close to a limit, scrape a web page or estimate the window from local logs. It can wait until Anthropic says the allowance has actually been rejected, record Anthropic's own reset time, replay that request to Bedrock, and remain on Bedrock until the window opens again.

There is one subtle but important rule here. A plain HTTP 429 is not enough. Anthropic also uses 429 responses for temporary server side throttling and other limits, so automatically converting every 429 into paid Bedrock traffic would be both technically sloppy and potentially expensive. The router should switch only when the subscription specific quota state says that the included allowance is genuinely exhausted.

## 6. This is not the same as bypassing the Max limit

This distinction matters technically and contractually.

The design does not attempt to make Max deliver more than Anthropic has allowed. It does not rotate accounts, share one user's credentials across a team, modify Anthropic's quota headers, fake a reset or suppress a rejection. Once Max says no, that Max channel is treated as unavailable until Anthropic's own reset time. The next request is a separate paid request through Amazon Bedrock.

That is also directionally consistent with Anthropic's own guidance. Its current Max documentation tells users who hit their included limits that one option is to switch to metered API credits for intensive coding work. I am applying the same principle to a Bedrock account that an organisation already operates and governs.

There are still terms questions that a company should review before rolling this out widely. Anthropic markets Max as a plan for individual consumers, while Team and Enterprise are the products it points organisations toward. The Consumer Terms also prohibit account sharing and prohibit bypassing Anthropic's systems or protective measures, so one Max identity per actual engineer is a hard design assumption rather than an optional detail.

The Consumer Terms are more nuanced than simply saying that paid work is forbidden on a consumer account, and Claude Code is explicitly offered as part of Max. Nevertheless, a bank or other regulated organisation should make its own legal and procurement decision about whether individual Max subscriptions are an acceptable employee tool. A clever routing architecture does not make consumer and enterprise contracts identical.

## 7. Security controls can change the enterprise calculation, but not the contract

One of the normal reasons to insist on an enterprise AI product is data control. In many environments, however, outbound security controls already inspect what leaves the laptop and prevent sensitive categories of information from being sent to the model provider. Code can be allowed while passwords, secrets and personally identifiable information are filtered before egress.

That materially changes the risk calculation for an interactive coding use case. If sensitive data cannot leave the endpoint in the first place, some of the value normally attributed to the model provider's enterprise boundary is being delivered by your own security architecture instead.

It does not remove every difference. Consumer and commercial Anthropic products have different administration, data processing, retention and governance arrangements, and those still matter. The lesson is simply that companies should identify which controls they genuinely need from the AI vendor and which controls they already enforce elsewhere, rather than assuming the most expensive commercial route is automatically the safest architecture.

## 8. Building the first version of the router

I have called the experiment **Claude Burst**.

The first version is deliberately small and Mac only. Claude Code points to a localhost gateway, the gateway forwards requests to Anthropic using the engineer's existing Max login, and it watches only the authoritative quota metadata. When Anthropic rejects the subscription allowance, Claude Burst records the reset time and replays the request against the mapped Claude model on Amazon Bedrock.

The Bedrock credential is kept in macOS Keychain. The current MVP uses a Bedrock API key and Amazon's Anthropic compatible Messages endpoint, which keeps the translation layer much smaller than converting every Claude Code request into the older Bedrock InvokeModel wire format.

The most important behaviour is deliberately conservative: ordinary 429s do not trigger paid overflow. The code only changes routes when the subscription specific limit is rejected, and it moves back to Max after the exact reset timestamp supplied by Anthropic. There are obvious next steps before this becomes enterprise software. AWS SSO and role assumption should replace long lived Bedrock API keys, model compatibility needs broader testing, and the gateway needs soak testing against new Claude Code beta capabilities as Anthropic releases them. That is why I am treating this as an open source experiment rather than pretending the first build is a finished platform.

## 9. The first version worked, and that was not the same as it being trustworthy

The core routing logic was sound and the first test suite proved the interesting case: a rejected Anthropic request really did get replayed to Bedrock with the model remapped and the OAuth beta header stripped. What it did not have was a way to answer a much more mundane question. If something goes wrong at three in the morning, what actually happened.

The original router had one real log statement, written when overflow activated, recording the claim, the reason and the reset time. Every other branch that could go wrong called an HTTP error response and returned. A body that failed to read, a request over the configured size limit, a Bedrock key missing from Keychain, a model with no entry in the model map, a network error calling either upstream, a failed write to the metrics file. All of these returned a sensible status to Claude Code. None of them left a trace anywhere on disk.

That is a reasonable shape for a proof of concept. It is not a reasonable shape for something that sits between a paid subscription and a metered AWS bill and decides, unattended, which one an engineer's inference goes through.

## 10. Every request now gets a start line and a done line

The fix I wanted was simple to state and slightly fiddly to get right without disturbing the streaming response path. Every request, successful or not, now gets a short id, generated once at the top of the handler and carried through the request's context. Two lines get written for that id: one when the request starts, one when it finishes, and the finishing line always carries the actual HTTP status the client received and how long the request took.

The fiddly part was getting the true final status out of a handler that streams a response body directly rather than building it up and writing it once. I wrapped the response writer in a small type that records the first status code written, whether that happens through the header call or through the first write, and still passes flushing through so the server sent events relay loop keeps working exactly as before. The wrapper does nothing except remember what already happened. It does not change behaviour.

With that in place, a panic recovery wrapper became almost free to add. If anything below panics, a stack trace goes into the log against that same request id, the client gets a clean server error instead of a hung connection or a crashed process, and the done line still gets written. A local gateway that a developer is relying on to keep working through their session should not be one unhandled error away from taking Claude Code down with it.

## 11. Naming the failure instead of hiding it

Once every request had a start and a done line, the next pass was going through each early return and giving the operator something to search for. Not a generic exception message, a labelled stage. Reading the body. Building the outbound request. The upstream call itself. Loading the Bedrock key. Mapping the model. Each of those now writes one line naming the stage, the route, and the underlying error before the response goes back to the client.

This matters more than it looks. "Bedrock overflow unavailable" told a user something was wrong. It did not tell whoever runs the fleet of these gateways whether the problem was a missing environment variable, a Keychain entry that got wiped by an OS update, or Bedrock itself being unreachable. Those are three different follow up actions, and previously they all looked identical from the log file, because there was no log file entry at all.

## 12. A metrics failure must never become a request failure

The structured metrics file records the route, model, tokens, estimated API equivalent cost, and the reset claim when one applies. It is written after the response has already been sent to Claude Code. That ordering matters, because it means a failure to write that file, whether from a full disk, a permissions problem, or a configured path that turned out to be a directory, must never turn into a failure of the actual inference request. The old code silently discarded that write error. It still silently continues past the failure, which is correct, but it now logs the error first, so a slow, quiet loss of metrics does not go unnoticed until someone tries to summarise usage and finds three weeks missing.

The same principle applied to the small local state file that records whether the gateway is currently in overflow. Reading a corrupted state file or failing to write an updated one previously failed silently. Both now log what happened and continue with a safe default rather than either crashing or pretending nothing occurred.

## 13. Writing tests for the failures, not just the feature

The original test suite proved the feature. Six new tests prove the surrounding behaviour that makes the thing operable.

One confirms that the start and done lines for a single request share the same id, since a log you cannot correlate is barely better than no log. One sends an oversized body and checks both the rejection and the specific log line explaining why. One removes the Bedrock credential entirely and checks that the failure is a clear, immediate error with the stage named, rather than a hang. One sends a model with no Bedrock mapping and checks the error names the offending model. One sends a plain server error from the primary upstream and checks it passes straight through without being mistaken for a subscription limit, since that distinction is the entire point of the conservative failover design. One points the metrics path at a directory instead of a file and confirms the actual inference request still succeeds while the failure gets logged. The last one hands the handler a request body that panics on read, a reasonable stand in for a corrupted transport, and confirms the panic is caught, logged with a stack trace, and turned into a clean error rather than escaping.

All eleven tests pass together, along with a vet check and a race enabled run. Test coverage on the router package sits at fifty two percent, which is an honest number for a project that still has real gaps in it, among them AWS SSO support and broader Bedrock model compatibility testing, rather than a number inflated by testing getters and setters.

## 14. What did not change

The privacy design from the first version stayed exactly as it was. Prompts, source code, tool inputs and model outputs are still never written to disk anywhere, including in the new logging. Logging everything here meant every code path and every failure mode, not the content of the requests themselves. For something that sits in front of a bank's engineers, that is not a detail to change casually, and it is not a decision this pass tried to revisit.

## 15. The experiment I actually want to run

The interesting number is no longer the theoretical figure from that first comparison. It is the real blended cost of a strong developer over time.

For each engineer I want to measure the API equivalent value consumed while Max is serving requests, the number of times Max actually reaches a hard allowance, the amount of time spent on Bedrock overflow, and the real AWS cost of that overflow. After a month, the economics should be obvious.

If a fixed subscription absorbs most interactive coding demand and Bedrock only catches occasional bursts, then paying API rates for every token is economically difficult to justify for this workload. If heavy engineers live on Bedrock for large parts of the week, the apparent Max advantage will shrink and a commercial enterprise arrangement may be the better answer. Either result is useful because it replaces vendor pricing assumptions with measured workload economics, and now, when that experiment produces a surprising number or an unexpected failover, there will be a log line explaining why.

## 16. AI pricing is becoming an architecture problem

The broader lesson is that model selection is no longer the only meaningful cost decision in AI engineering. The route to the model can matter as much as the model itself.

Two requests can reach essentially the same Claude intelligence and have radically different economics because one sits inside a fixed subscription allowance and the other is metered token by token. As AI agents become capable of consuming enormous amounts of context during an ordinary developer's day, those pricing boundaries become architecture boundaries.

I learned the pricing half of this from a comparison that, at first glance, looked like a measurement error: a subscriber on a flat monthly plan whose usage translated into a large multiple of that plan's cost at public API rates. I learned the engineering half of it by building something small enough to feel finished after a weekend, and then finding out how much further it is from working to trustworthy. The next logical step is not to complain that the pricing makes no sense, but to design around the commercial reality that Anthropic has created, and to build the boring, unglamorous parts, the logging, the error handling, the tests for the failure paths, with the same care as the interesting routing decision at the centre of it.

Use the included capacity. Respect the limit. Pay for the burst. Measure everything. Log everything that goes wrong so you never have to guess.

That may turn out to be a much better way to buy coding intelligence.

## References

1. Anthropic, Plans and Pricing: https://claude.com/pricing
2. Anthropic, What is the Max plan?: https://support.claude.com/en/articles/11049741-what-is-the-max-plan
3. Anthropic, Use Claude Code with your Pro or Max plan: https://support.claude.com/en/articles/11145838-use-claude-code-with-your-pro-or-max-plan
4. Anthropic, Other LLM gateways and subscriptions: https://code.claude.com/docs/en/llm-gateway
5. Anthropic, Gateway protocol reference: https://code.claude.com/docs/en/llm-gateway-protocol
6. Anthropic, Claude Code error reference: https://code.claude.com/docs/en/errors
7. Anthropic, Consumer Terms of Service: https://www.anthropic.com/legal/consumer-terms
8. Anthropic, Claude Code on Amazon Bedrock: https://code.claude.com/docs/en/amazon-bedrock
9. AWS, Inference using the Anthropic Messages API: https://docs.aws.amazon.com/bedrock/latest/userguide/inference-messages-api.html
10. AWS, Use an Amazon Bedrock API key: https://docs.aws.amazon.com/bedrock/latest/userguide/api-keys-use.html
