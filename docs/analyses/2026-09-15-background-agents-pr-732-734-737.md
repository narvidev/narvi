# Reprises utiles de background-agents dans Narvi

Analyse du 15 septembre 2026.

> **Statut : historique, non normatif.** Ce document enregistre comment une
> conclusion a été atteinte, à une date et contre un commit donnés. Ce qui en
> est ressorti vit dans `docs/IMPLEMENTATION_PLAN.md` (Steps 172-176, plus les
> amendements des Steps 136, 137, 142 et 148, PR #289) et dans
> `docs/TECHNICAL_PLAN.md`. En cas de divergence, les plans font foi et ce
> document est périmé par construction.

## Conclusion

Les reprises prioritaires portent sur **la validité des reviews lorsque leur base change**, **le cycle de vie des stacks**, **la compatibilité des snapshots**, puis **les budgets sans progrès**. Le transport GitHub Checks proposé par la PR 737 est utile, mais sa politique de merge doit être adaptée à Narvi : un résultat non évalué ne doit pas devenir une autorisation d'auto-merge.

Une partie importante de la release est déjà couverte par le code ou les phases 13–16 du plan. Il faut compléter ces travaux, pas créer une seconde implémentation. La PR 734 ne révèle aucun correctif fonctionnel à porter : le décodage actuel de Narvi accepte déjà le SHA nul.

## Périmètre et niveau de vérification

- Narvi analysé au commit `d4ae0328aca8c8e23e9927040f3ec03fada2c051`, branche locale `docs/claims-parity-and-k8s-provider`.
- PR 732 — Release: main → stable : fusionnée le 14 septembre 2026 à 16:21:45 UTC, commit de merge `ed8e620044c25fa4b6d3df8cf7acf943d64d77fa`. La fusion est vérifiée ; l'état actuel des services en production n'a pas été sondé.
- Les 43 PR intégrées à cette release ont été identifiées dans ses 218 commits. La liste paginée GitHub contient **383 fichiers** ; le texte de présentation en annonce 347. L'inventaire API et les patches ont donc servi de référence, avec lecture ciblée des chemins pertinents pour Narvi. Ce rapport n'est pas une revue exhaustive de chaque ligne des 383 fichiers.
- Les quatre PR suivantes étaient ouvertes, non draft, au dernier contrôle. Leurs SHA n'ont pas changé pendant l'analyse.

| PR | Commit analysé | État des checks au dernier relevé |
|---|---|---|
| PR 734 — SHA de base nul | `4d2ceeabf1b980fd222fcb8e43738e56ba1a6dc0` | Checks terminés, succès ou étapes ignorées |
| PR 735 — Debounce des événements stacked | `5acd6463655227b5c2e17d035a51a78678fbcc17` | Test d'intégration control-plane encore en cours ; aucun échec remonté |
| PR 736 — Contexte, résultat et acceptation de review | `d65f68b4190079260ef92a99a1292d5a1ac55f1d` | Checks terminés, succès ou étapes ignorées |
| PR 737 — Client GitHub Checks | `2b3883be0eb20a11d1eaf7e3222fa626db711a0c` | Checks terminés, succès ou étapes ignorées |

Les indications « présent » ci-dessous reposent sur le code, et non sur la seule présence d'une ligne dans le plan. Plusieurs lignes historiques de celui-ci décrivent encore des lacunes désormais corrigées, notamment la remontée `agentVersion`/`imageDigest`.

## 1. Reviews : reprendre le contexte complet avant la publication des checks

### PR 736 : un manque réel, avec une portée supérieure aux stacks

Narvi persiste un `HeadSHA` sur son verdict. Son moteur d'éligibilité vérifie que celui-ci égale le SHA courant. Il ne compare ni le commit de base, ni les ancêtres d'une stack, ni une version de politique dans cette décision.

Preuves : `internal/domain/reviewverdict/record.go`, `internal/domain/autoapproval/eligibility.go`, `internal/app/decisioninbox/revalidate.go`. Le décodeur GitHub lit actuellement `base.ref`, mais pas le `base.sha` direct, dans `internal/adapters/outbound/githubapi/adapter.go`.

**Cas concret :** une PR B a été évaluée sur la branche de A ; A change ou B est retargetée, sans changement du head de B. Le seul test d'égalité des heads peut encore présenter l'ancien verdict comme courant. Les autres contrôles de merge restent actifs, mais ne prouvent pas que la review couvre le nouveau contexte.

À reprendre de la PR 736 :

1. Un contexte contenant dépôt, PR, `headSHA`, `baseRef`, `baseSHA`, chaîne ordonnée des ancêtres et version de politique.
2. Un identifiant de tentative distinct de ce contexte : deux tentatives sur le même code ont le même contexte, mais une seule doit pouvoir publier l'état courant.
3. Une distinction explicite entre évaluation terminée et `not_assessed`, sans valeur de risque inventée pour le second cas.
4. Une acceptation humaine liée au verdict exact, à sa tentative et à son contexte, avec auteur, justification et révocation auditables. Un nouveau verdict ou contexte la rend inapplicable ; l'acceptation ne réécrit pas le risque en « faible ».

La chaîne doit être propre à chaque PR, pas une révision globale de stack : modifier PR5 invalide PR5 et ses descendants, sans invalider PR1–4. L'identité de la stack GitHub ne suffit pas, puisqu'une restructuration peut la dissoudre puis la recréer.

**Adaptation Narvi :** étendre l'enveloppe persistée autour de `review.Verdict`, conserver son `Shippable` recalculé côté serveur, et faire consommer le même contexte par l'inbox, l'auto-merge, les futures avances de train et la publication GitHub. Ajouter seulement des types ou un badge laisserait le problème entier.

Le plan §21.1 reconnaît aujourd'hui la limite liée à la composition des stacks ; §39.3 réutilise la fraîcheur fondée sur le head. Ces sections doivent être amendées ensemble. La PR 736 elle-même ne livre encore que contrats et fonctions pures, sans persistance ni endpoints.

### PR 737 : reprendre le transport, adapter la politique

Narvi publie actuellement des reviews formelles et des labels via `internal/adapters/outbound/githubapi/verdictnotifier.go`. Aucun publisher de check `narvi/review` n'a été trouvé. La lecture de checks de CI existe déjà, mais ne constitue pas une publication de review.

Un check permettrait de représenter file d'attente, exécution, verdict périmé et résultat terminal, et de rendre cette review obligatoire par règle GitHub. La PR 737 fournit uniquement le client et ses tests ; aucun webhook ne l'appelle encore. Elle conserve aussi des types locaux provisoires qui devront converger avec la PR 736.

Pour Narvi, prévoir :

- une publication par l'outbox existante, avec identité du check conservée et contrôle de la tentative/contexte avant émission ;
- une sélection par SHA **et GitHub App**, pour ne pas adopter le check homonyme d'une autre application ;
- un nouveau check lorsqu'une review recommence après un résultat terminal ;
- des erreurs visibles et des reprises distinguant permissions, limitation de débit et panne temporaire ;
- une authentification d'installation disposant de `checks:write`, à vérifier explicitement pour Narvi. La PR 737 indique que cette permission manque actuellement à l'installation upstream ; cela ne renseigne pas celle de Narvi.

Un lookup suivi d'une création n'empêche pas deux publications concurrentes de créer deux checks. Le client upstream laisse aussi au caller le rejet d'une tentative obsolète. L'actor et la persistance Narvi doivent fournir cette coordination, y compris lorsqu'un même SHA correspond à un contexte de base différent.

**Politique à ne pas transposer implicitement :** upstream publie `not_assessed` en `neutral` et laisse merger ; ses risques faibles et moyens passent, et un risque élevé accepté passe aussi. GitHub accepte effectivement `neutral` parmi les résultats satisfaisant un required check ([documentation GitHub](https://docs.github.com/en/pull-requests/how-tos/merge-and-close-pull-requests/troubleshooting-required-status-checks)). C'est une décision produit, pas une particularité d'affichage. Narvi doit conserver ses conditions `Shippable`, couverture, CI et chemins sensibles ; `not_assessed` ne doit jamais satisfaire son auto-merge. L'effet sur le merge manuel et les required checks mérite une politique explicite par dépôt.

L'écriture via Checks requiert les permissions appropriées de la GitHub App ([documentation GitHub](https://docs.github.com/en/rest/guides/using-the-rest-api-to-interact-with-checks)). La migration doit également traiter les anciennes reviews `REQUEST_CHANGES` encore bloquantes : publier un check réussi ne les retire pas.

## 2. Stacks : couverture actuelle partielle, compléter les événements

Narvi sait déjà lire le contexte natif d'une stack et appeler `RegisterPRStack`. Le plan §17.6 limite cet usage à la paire PR originale/correctif sentinel, et §39 prévoit les trains plus longs. Ce socle ne fournit pas le cycle de vie livré par la PR 724.

### PR 734 : aucun portage fonctionnel nécessaire

Le `SHA string` de `stackResponse` accepte déjà `null` via `encoding/json`, avec chaîne vide comme valeur inconnue. Un test temporaire exécuté contre le véritable `GetPullRequest` a validé les trois formes : SHA absent, nul et renseigné.

Un test permanent de non-régression sera utile avec le prochain travail sur les stacks. En revanche, il n'y a pas lieu de modifier ce décodeur pour reproduire un correctif Zod. La valeur vide doit rester « inconnue » dans le futur calcul de validité, sans être assimilée à une preuve de fraîcheur.

### PR 735 et 724 : reprendre le cycle de vie, pas seulement un délai

L'ingress Narvi route `synchronize` vers un timer persistant ; les autres événements PR sont traités pour des usages précis, notamment les labels et la fermeture. Aucun chemin `stacked` comparable n'a été trouvé dans `internal/adapters/inbound/github/handler.go`. Le retarget est prévu au Step 142, pas encore implémenté dans ce chemin.

À ajouter :

- persister le travail avant l'accusé de réception du webhook ;
- relire l'appartenance réelle, y compris après retrait/dissolution ;
- invalider immédiatement les décisions devenues inapplicables ;
- différer seulement le lancement d'une nouvelle review jusqu'à la fin de la période calme ;
- abandonner une ancienne tentative si un nouvel événement la remplace pendant une lecture GitHub ;
- conserver le travail en échec pour reprise via les timers Postgres existants.

Le **diff actuel** de la PR 735 est plus précis que son résumé : l'alarme reste immédiate pour révoquer une approbation devenue contraire à la politique ; seul le lancement attend `launchAfter`. Reporter les deux actions ensemble laisserait l'ancienne approbation utilisable pendant le délai.

Le debounce reste **par PR**. Dix PR distinctes peuvent encore déclencher dix reviews au terme du délai ; cette PR ne fournit ni ordonnanceur global de stack, ni sérialisation de ses membres. Ne pas annoncer cette propriété comme acquise.

Enfin, la PR 724 évalue la branche cible ultime d'une stack pour certaines politiques, tout en reviewant le diff incrémental contre le parent direct. Narvi prévoit en §24.8 de différer les branches non trunk, sauf besoin de verdict. Il faut décider explicitement comment ces règles s'articulent avant d'étendre le Step 142 ; ne pas remplacer silencieusement la base directe par la base ultime partout.

## 3. Release 732 : couverture et décision de reprise

| PR incluses | Apport | État de Narvi et décision |
|---|---|---|
| 679, 690, 698, 699 | MCP, tokens/OAuth, suivi peu bavard, lecture de verdict | **Absent comme API externe.** La phase 16 prévoit stabilité des contrats, authentification native et reprise du flux. Si les clients externes sont retenus, ajouter un adaptateur fin vers les services existants, scopes/révocation, approbation de plan conservée, état compact avec délai conseillé et transcript sur demande. Réutiliser la lecture des verdicts Postgres ; aucun second magasin. Le serveur OAuth MCP n'est pas couvert automatiquement par le futur device flow natif. |
| 683, 684 | Catalogue de modèles ; OpenCode 1.18.30 | **À actualiser ensemble et vérifier.** Narvi utilise encore 1.17.15 dans la configuration et la CI ; son snapshot de catalogue n'inclut pas Astra/Fable 5.1. Rafraîchir depuis le runtime retenu, avec capacités/efforts et coûts cohérents. Ne pas reprendre les endpoints de modèles privés upstream. |
| 685, 687, 688, 689 | Trains Linear : feuilles, dépendances déclarées, attribution aux sessions, issue sans PR | **Prévu, non implémenté.** Compléter Steps 145–147 : décomposition en feuilles, blockers hérités et résolus vers les feuilles, graphe réconcilié côté serveur, association lien→session, fin sans PR terminale et notification idempotente, routage retiré après Stop. §39 couvre déjà plusieurs invariants, mais pas tous ces détails. La limite upstream à deux niveaux vient de sa requête Linear ; ce n'est pas un besoin Narvi. |
| 686, 697 | Secrets juste après création ; réglages personnels/déploiement | **Améliorations UX secondaires.** Le formulaire Narvi revient à la liste après création ; Settings reste thématique. Rendre le chemin vers les secrets utiles évident et séparer les portées visuellement, en réutilisant les scopes/RBAC actuels. Les secrets propres à une automation seraient un changement de modèle supplémentaire, pas un simple déplacement de formulaire. |
| 692, 702 | Ponytail opt-in ; versions des plugins épinglées | **Choix facultatif.** Narvi n'a pas ce mode produit. Épingler tout plugin fourni par la plateforme si ajouté, nettoyer les installations désactivées à la restauration, conserver une sélection explicite par dépôt. Pas de dépendance obligatoire à introduire pour la parité. |
| 703 | Déclenchement sur un check CI nommé | **Manque réel, plus large qu'un champ.** `GitHubTriggerConfig` ne contient que event/action/label ; `MatchesGitHubTrigger` n'a pas d'appel de dispatch en production. Brancher d'abord l'ingress sur le moteur existant, puis normaliser `check_run` et `status` avec nom/conclusion et déduplication. Pour un commit status, distinguer une branche contenant le commit de celle dont il est encore le tip. |
| 709 | Plancher de durée restante avant rotation réellement configurable | **Déjà prévu au Step 137.** Intégrer son test de monotonie : relever le plancher doit effectivement avancer la rotation. Ne pas copier les constantes de durée du fournisseur upstream. |
| 710, 715, 718 | Version embarquée, refus de snapshot incompatible, migration des non versionnés | **Reprise prioritaire complémentaire aux Steps 136–139.** Narvi remonte déjà agentVersion/imageDigest, mais n'associe pas une version de runtime au snapshot pour décider de sa compatibilité. Stocker cette provenance au mint et la conserver à chaque restauration ; une nouvelle date de démarrage ne rajeunit pas les binaires du snapshot. Refus visible, continuité/récap traités par Step 139. La migration en pourcentage des snapshots sans version n'est nécessaire que s'il existe réellement une lignée ancienne à migrer. |
| 711 | Diagnostic des erreurs de fournisseur/runtime | **Partiellement couvert.** Modal conserve code/opération/message et distingue les erreurs réseau. OpenCode ne modélise dans `data` que `isRetryable` et `statusCode`. Ajouter les informations diagnostiques utiles et expurgées, sans réintroduire les heuristiques textuelles de retry là où Narvi dispose déjà d'un signal typé. |
| 713, 714 | Identifier les manifests imposant setup ; expliquer le choix d'image | **Petite reprise utile.** Narvi journalise déjà skip/delta/full et certaines erreurs, mais son choix d'image revient silencieusement au défaut pour plusieurs états ordinaires. Persister une raison de sélection/fallback et exposer les fichiers de dépendances ayant changé. Réutiliser la collecte et le journal d'événements existants. |
| 712, 725 | Limites vidéo visibles à l'avance ; quota récupérable | **Partiellement couvert.** Les uploads Narvi ont déjà limites configurables par fichier/session, contrôle à la confirmation et nettoyage des abandons. Le chemin screenshot/vidéo avec quota de places n'est pas le même produit. Retenir la suppression explicite avec restitution de quota et l'information avant capture quand cette capacité sera exposée. |
| 716 | Clarifier la continuité de branche lors d'une reprise | **Revue de documentation ciblée.** Le cycle git stash/checkout/pop existe dans Narvi. Reprendre les scénarios de continuité comme critères de recette, sans importer l'ADR spécifique au runtime upstream. |
| 717, 726 | Budget par objectif puis depuis le dernier progrès observable | **Manque à intégrer à la phase 15.** `loopguard` borne les itérations d'une étape de workflow, pas l'ensemble du travail sans progrès d'une session. Mesurer temps actif/coût/tentatives depuis une PR créée/fusionnée confirmée par le fournisseur ou une reprise humaine explicite ; contrôle serveur même si l'agent n'utilise pas son outil. Conserver séparément les plafonds absolus des Steps 148/150. |
| 719 | Flags d'outils identiques à la création et à la restauration | **Invariant déjà largement couvert par la structure.** Create et Restore transportent le même `SessionConfig`, reconstruit via le même assembleur. Garder un scénario sur restauration réelle pour toute nouvelle capability ; le retrait d'un outil doit effacer une éventuelle installation ancienne. Aucun mapping d'env upstream à recopier. |
| 720, 721, 722, 723 | Sentry, feedback, replay, sourcemaps | **Adaptation facultative.** Narvi possède déjà OTel/OTLP, ce qui couvre une partie de l'observabilité serveur, pas la capture des erreurs front ni le feedback. Un branchement configurable peut compléter ce socle. Ne pas reprendre par défaut le replay non masqué ni le projet Sentry partagé upstream ; appliquer le modèle de confidentialité et d'hébergement Narvi. |
| 724 | Création et cycle de vie des stacks natifs | **Partiel aujourd'hui.** Voir section 2 : base ultime, changements de membership, déduplication durable et invalidation doivent compléter les primitives déjà présentes. La release ne fournit pas un système général de restack/merge automatique. |
| 727 | Outils d'ancrage pour reviews dédiées | **Pas de portage AST systématique.** Narvi dispose déjà de `MatchPosition` et d'un fallback LLM pour les findings. Retenir les vérifications de head/diff et de résultat publié ; ajouter des parsers AST uniquement si une capacité d'édition structurée les exige. |
| 728, 729 | CA EKS de staging ; sessions STS de trois heures | **Spécifiques à l'infrastructure upstream.** Aucun certificat à copier ni allongement uniforme des credentials. Narvi a sa propre fédération d'identité et ses durées centralisées. |
| 730 | Correctifs issus de recette staging | **À utiliser comme corpus de scénarios.** Narvi filtre déjà les résultats provider par génération et construit les clés d'upload côté serveur avec lecture bornée à la session. Le delta SSE en revanche est pertinent lors de la montée OpenCode ; voir ci-dessous. Les races révision/approbation de plan, Stop des enfants, nettoyage des outils restaurés et l'absence de PR devront accompagner MCP/trains. |
| 731 | Help Center aligné avec les fonctionnalités réelles | **Reprendre la discipline.** Mettre à jour les guides Narvi au moment de chaque livraison, en distinguant comportement disponible et planifié. Ne pas importer les parcours upstream. |
| 691, 693, 694, 695, 696 | Mises à jour Hono/Vitest/js-yaml/undici/Next | **Pas de reprise groupée.** Narvi utilise Go + Vite ; Vitest est déjà en 4.1.11 dans `web/package.json`. Évaluer uniquement les dépendances réellement présentes, sans importer une pile web étrangère. |

### Point de migration OpenCode à ne pas manquer

Dans `internal/adapters/outbound/opencode/sse.go`, Narvi ignore explicitement `message.part.delta`, considéré comme redondant avec les textes cumulatifs de `message.part.updated`. La PR 730 corrige précisément le traitement de ces deltas pour OpenCode 1.18.30.

Une mise à jour de version doit vérifier contre le vrai binaire : assemblage par message/part, absence de duplication avec les mises à jour complètes, séparation des sessions parentes/enfants et transcript final après reconnexion. L'observation justifie ce test de migration ; elle ne démontre pas une régression du runtime 1.17.15 actuellement épinglé dans Narvi.

Repères : `internal/platform/config.go`, `.github/workflows/ci.yml`, `internal/app/modelcatalog/snapshot.json`.

## 4. Ordre recommandé dans le plan Narvi

Ces priorités désignent le travail utile pour Narvi ; elles ne qualifient pas la sévérité de bugs dans les PR upstream.

| Priorité | Lot concret | Rattachement conseillé | Critère d'acceptation principal |
|---|---|---|---|
| P1 | Contexte de review, résultat non évalué, tentative courante | Amender §21.1 et §39.3 ; nouveaux Steps dédiés, sans renuméroter l'existant | Un changement de base/ancêtre avec head identique invalide l'éligibilité ; une ancienne tentative ne remplace jamais la courante |
| P1 | Cycle de vie `stacked` et retarget | Compléter Step 142 et §17.6 ; réutiliser les timers actuels | Invalidation immédiate, lancement après calme, webhook rejoué idempotent, reprise après panne |
| P1 | Version des snapshots et continuité | Compléter Steps 136–139 | Une restauration ne blanchit pas la provenance d'un vieux runtime ; fallback annoncé et contexte récupérable |
| P1 | Budget sans progrès | Compléter Steps 148/150, en plus des bornes absolues | Arrêt observable sans appel coopératif de l'agent ; une auto-déclaration de succès ne remet pas le compteur serveur à zéro |
| P1 lors du bump | OpenCode, SSE, catalogue et image cohérents | Lot de maintenance runtime, lié au corpus de contrats existant | Flux et transcript exacts contre le binaire retenu, y compris après restauration/reconnexion |
| P2, après contexte | Publisher `narvi/review` et acceptation humaine | Nouveau lot cohérent après les contrats de review | Pas de succès obsolète publié ; permissions/erreurs visibles ; acceptation périmée après changement de contexte |
| P2 | Trains Linear durcis | Compléter Steps 145–147 avant implémentation | Une feuille = un lien/session ; dépendances du tracker respectées ; arrêt et fin sans PR propagés |
| P2 | Automations sur check nommé | Compléter le dispatch GitHub encore manquant de §8.4 | Un seul lancement sur le check voulu ; statut pending et check homonyme non voulu exclus |
| P2, faible ampleur | Diagnostic d'image et fichiers imposant setup | Complément des fonctionnalités warm boot déjà livrées | La cause du cold boot reste lisible après rechargement ; les manifests responsables sont nommés |
| Selon décision produit | MCP/OAuth, UX settings/secrets, médias, Sentry/Ponytail | Phase 16 pour les clients ; lots UI/intégrations distincts | Pas de deuxième autorité sur sessions, autorisations, quotas ou verdicts |

Le premier lot de reviews doit fixer la sémantique commune, puis le publisher peut s'y brancher. Pour les trains, réutiliser le moteur d'avance prévu plutôt qu'introduire un ordonnanceur autonome à côté des actors. Pour les snapshots, compléter la phase 13 déjà conçue plutôt que transposer une logique Durable Object étrangère.

## 5. Vérifications réalisées et limites

- Test temporaire contre `githubapi.GetPullRequest`, lancé avec `go test -race` et un overlay Go : `base.sha` absent, nul et renseigné passent tous. Le transport HTTP était simulé, sans requête GitHub réelle. Aucun fichier de production ni test du dépôt n'a été modifié pour cette preuve.
- `go test -race ./internal/domain/autoapproval ./internal/domain/loopguard ./internal/domain/upload` : succès.
- Les SHA et checks des PR 734–737 ont été relus après l'analyse ; aucun nouveau head n'était apparu.
- Aucun test de bout en bout Modal, Linear, MCP ou Checks n'a été exécuté pour Narvi. Les résultats de recette rapportés dans la PR 730 concernent le projet upstream, pas Narvi.
- Aucune reprise fonctionnelle n'a été appliquée au moment de la rédaction et les deux plans étaient inchangés.

## 6. Ce qui en est sorti

La PR Narvi #289 a déposé les Steps 172-176 et amendé les Steps 136, 137, 142 et 148. Onze affirmations de ce document sur le code de Narvi ont été revérifiées indépendamment avant dépôt ; les onze tenaient, et ses citations de Steps et de sections résolvaient toutes.

Deux points ont été renforcés à cette occasion. Ce document écrivait prudemment que les autres contrôles de merge « ne prouvent pas que la review couvre le nouveau contexte » ; la vérification a établi qu'**aucune garde sur la branche de base n'existe**, et que le Step 142 qui la fournirait n'est pas livré — rien en aval n'arrête le cas. Et le point `not_assessed` → `neutral` a été nommé pour ce qu'il est : un échec qui se rend comme un état normal et confiant, la même forme que trois défauts corrigés dans Narvi la même semaine.
