# Absences dans la documentation — reprise de background-agents

Inventaire du 15 septembre 2026, après la PR Narvi #289.

> **Statut : historique, non normatif.** Ce document est un constat daté, contrôlé
> contre `main` au commit `071f10721f0fa205897abc3f2c05398c544c9082`. Il vieillit
> par construction : plusieurs de ses cases ont été fermées après sa rédaction, et
> la trace de ce qui a été fait vit dans les plans, pas ici. Voir la section finale
> pour ce qui a bougé depuis.

## Périmètre et lecture

Cet inventaire rassemble **tous les écarts identifiés dans l'analyse de la release 732 et des PR 734 à 737** qui restaient absents, incomplets ou à décider dans la documentation de Narvi à cette date. Il couvre les 43 PR intégrées à la release et les quatre PR suivantes. Il ne constitue pas un inventaire de toutes les fonctionnalités manquantes de Narvi ni une revue exhaustive de chaque ligne upstream.

La [PR Narvi #289](https://github.com/narvidev/narvi/pull/289), fusionnée le 15 septembre 2026, modifie `README.md` et `docs/IMPLEMENTATION_PLAN.md`. Elle ne modifiait pas `docs/TECHNICAL_PLAN.md`.

Les cases ci-dessous désignent du **travail documentaire**, pas des développements autorisés ou déjà réalisés.

- **Absent** : aucun lot explicite ne reprend le besoin dans les deux plans.
- **Partiel** : le besoin est inscrit dans le plan d'implémentation, mais sa spécification ou certains critères restent incomplets.
- **Option** : capacité upstream absente, dont l'adoption demande une décision produit ; ce n'est pas automatiquement un défaut de Narvi.
- **Traçabilité / recette** : référence, publication ou critère de validation à conserver ; cela ne démontre pas un bug dans le code actuel.

## Synthèse

| ID | Écart | Statut à la rédaction | Rattachement proposé |
|---|---|---|---|
| A01 | Serveur MCP externe, outils, authentification et suivi des sessions | Absent ; adoption à décider explicitement | Lot MCP dédié, articulé avec Steps 157–159 |
| A02 | Trains : feuilles, dépendances héritées, sessions, Stop et fin sans PR | Partiel | Steps 145–147 et §39 |
| A03 | Diagnostic exploitable des erreurs runtime/fournisseur | Absent comme complément planifié | Adaptateur OpenCode et documentation de diagnostic |
| A04 | Spécification commune des reviews, Checks et acceptation humaine | Partiel | Steps 173–174 ; §21.1, §39.3 et publication GitHub |
| A05 | Politique des stacks et critères de concurrence/reprise | Partiel | Step 142 ; §17.6, §24.8 et §39 |
| A06 | Politique de compatibilité des snapshots | Partiel | Steps 136–139 ; §35 et cycle de restauration |
| A07 | Sémantique du budget sans progrès | Partiel | Step 148 ; §40, en lien avec Step 150 |
| A08 | Parcours secrets après création et portées des réglages | Option absente | Guides web et paramètres |
| A09 | Mode plugin fourni par la plateforme et cycle de vie des plugins | Option absente | Configuration par dépôt et restauration |
| A10 | Suppression de médias, quota récupérable et limites avant capture | Option / complément absent | §28 et éventuel parcours de capture |
| A11 | Erreurs navigateur, feedback, sourcemaps et replay | Option absente | Observabilité front et configuration du déploiement |
| A12 | Publication de l'analyse, référence warm boot et suivi des guides | Traçabilité incomplète | Documentation sur main |
| A13 | Corpus de recette transversal issu de la release | Critères complémentaires à inscrire | Lots concernés, sans campagne de tests séparée |

## A01 — MCP : le lot complet est absent

**Origine : PR 679, 690, 698, 699 et scénarios de la PR 730.**

La phase 16 décrit des prérequis : contrats stables (157), authentification de client natif (158), flux d'événements reprenable (159). Elle ne décrit **ni un serveur MCP Narvi, ni sa surface d'outils, ni son authentification propre**. Les mentions de MCP dans §27 concernent des serveurs consommés depuis la configuration OpenCode du sandbox : le sens d'intégration est différent.

### A01.1 — Décision produit et interface

- [ ] Inscrire une décision explicite sur l'exposition de Narvi à des clients MCP existants. Cette décision peut être prise indépendamment d'un éventuel client Narvi Desktop.
- [ ] Définir le point d'entrée, le transport, les versions de protocole prises en charge et les erreurs retournées au client.
- [ ] Prévoir un adaptateur vers les services applicatifs existants, avec les mêmes contrôles de dépôt, utilisateur, configuration du fournisseur et autorisations que les autres entrées.
- [ ] Documenter les capacités retenues et leur correspondance avec l'API Narvi. L'inventaire upstream comporte **14 outils**, regroupés par usage : découverte (dépôts, modèles) ; lecture des sessions (liste, détail) ; résultat et review (résultat, verdict) ; travail et suivi (déléguer, envoyer un prompt) ; événements et attente (sondage, attente d'inactivité) ; plan (lecture, approbation, rejet) ; arrêt.

Les noms d'outils upstream identifient la surface étudiée ; ils ne constituent pas une convention de nommage proposée pour Narvi.

### A01.2 — Authentification, consentement et révocation

- [ ] Choisir et documenter le support OAuth destiné aux clients MCP et la place éventuelle de tokens personnels. Le device flow natif du Step 158 ne couvre pas automatiquement ce besoin.
- [ ] Pour OAuth, préciser découverte du serveur, enregistrement/configuration des clients, autorisation avec consentement, échange et renouvellement des credentials, et révocation.
- [ ] Définir scopes de lecture/écriture, accès aux dépôts et vérification des droits sur chaque session et chaque outil ; filtrer la découverte des outils selon les droits.
- [ ] Décrire expiration, stockage protégé des credentials, invalidation des accès et gestion des autorisations accordées dans les paramètres utilisateur.
- [ ] Séparer cette authentification de l'OAuth de connexion à un fournisseur de modèles et des secrets internes du déploiement.

### A01.3 — Suivi peu bavard et résultat exploitable

- [ ] Spécifier un état compact de session avec un délai conseillé avant la prochaine lecture.
- [ ] Séparer état, résultat synthétique et transcript détaillé ; réserver le transcript paginé avec curseur à une demande explicite.
- [ ] Définir l'attente bornée et les états distinguant travail en file, travail en cours, attente d'approbation et fin réelle. Une file non vide ne doit pas être présentée comme inactive.
- [ ] Exposer les PR produites et le résultat de la dernière exécution, puis le verdict correspondant, sa fraîcheur et son absence éventuelle, depuis les données Narvi existantes.
- [ ] Articuler ces lectures avec les Steps 157 et 159 et avec le contexte de review du Step 173, sans créer un second stockage de sessions ou verdicts.

### A01.4 — Plan, révision et arrêt

- [ ] Conserver les étapes d'approbation de plan et les contrôles de droits lors d'une délégation ou d'un suivi MCP.
- [ ] Définir précisément l'effet d'une révision de plan mise en file pendant qu'une implémentation déjà approuvée est en cours : la nouvelle demande ne doit pas retirer rétroactivement l'autorisation du travail courant.
- [ ] Décrire l'effet de `Stop` sur l'exécution, la file d'attente et les éventuels enfants, en s'appuyant sur les transitions existantes.
- [ ] Ajouter les scénarios de recette MCP de A13 et un guide de connexion une fois la capacité retenue.

## A02 — Trains Linear : détails absents des Steps 145–147

**Origine : PR 685, 687, 688, 689 et 730.**

Le plan couvre déjà la mère qui planifie puis s'arrête, les dépendances déclarées, la validation du graphe, la réservation atomique de l'avance et le verdict recalculé côté serveur. Il reste à documenter :

- [ ] La décomposition jusqu'aux tickets **feuilles**, en conservant leur ascendance, sans lancer de travail de code pour les simples conteneurs.
- [ ] L'héritage des blocages portés par un conteneur et leur résolution vers les feuilles effectivement exécutables.
- [ ] La réconciliation du graphe soumis avec les relations du tracker avant de lancer les sessions. La seule validation de cohérence interne prévue au §39.2 ne décrit pas cette réconciliation.
- [ ] L'association persistée ticket feuille → lien de train → session, utilisée pour le suivi et le routage des messages.
- [ ] La résolution du prédécesseur et de la base effective de chaque lien, y compris après évolution du graphe.
- [ ] La fin d'un enfant **sans PR** : résultat terminal visible, notification idempotente au parent et traitement des successeurs pour éviter une attente infinie de verdict.
- [ ] Le routage des messages de suivi vers la bonne session et son retrait après arrêt ou clôture.
- [ ] La propagation d'un Stop explicite aux liens en attente et aux enfants actifs. Le plan couvre déjà l'arrêt d'un successeur sur verdict devenu bloquant ; cela ne définit pas tout le cycle d'arrêt du train.

La limite upstream à deux niveaux provient de sa récupération Linear. Aucune reprise automatique de cette limite n'est justifiée pour Narvi.

## A03 — Diagnostic runtime/fournisseur : complément non inscrit

**Origine : PR 711.**

Le socle est partiel : Modal conserve déjà des erreurs structurées ; les données d'erreur OpenCode modélisées se limitent principalement à `isRetryable` et `statusCode`.

- [ ] Définir les informations utiles conservées lorsqu'elles sont disponibles : message exploitable, code/opération, identifiant de requête fournisseur, modèle et version du runtime, contexte de l'hôte concerné.
- [ ] Préciser leur restitution dans le journal de session et le diagnostic opérateur, avec expurgation des secrets et limites de taille.
- [ ] Conserver la décision de retry issue des signaux typés ; ne pas remplacer ce socle par une classification fragile du texte des erreurs.
- [ ] Ajouter une recette où une erreur fournisseur permet de retrouver sa cause sans exposer credentials ou contenu sensible de requête.

## A04 — Reviews et Checks : lots inscrits, spécification encore incomplète

**Origine : PR 736 et 737.**

Les Steps **173 et 174** couvrent déjà le contexte complet, les tentatives, `not_assessed`, l'acceptation humaine, la publication par outbox et plusieurs règles importantes de Checks. Les absences restantes :

- [ ] Appliquer réellement l'amendement annoncé à **§21.1 et §39.3**. *(Fait depuis — voir la section finale.)*
- [ ] Décrire la consommation du même contexte et de la même tentative courante par l'inbox, l'éligibilité, l'auto-merge, les trains et le publisher GitHub.
- [ ] Préciser le contrôle de concurrence lors de la publication : une nouvelle tentative ou une nouvelle base avec head identique doit empêcher une ancienne émission d'écraser le résultat courant ; une course de création ne doit pas fabriquer deux identités actives.
- [ ] Définir la politique de **merge manuel / required check**, notamment pour `not_assessed`, et le comportement lorsque l'enforcement est désactivé. Le Step 174 interdit déjà l'auto-merge Narvi non évalué ; cela ne tranche pas toute la politique GitHub.
- [ ] Définir qui peut accepter ou révoquer un verdict, par quel parcours/API et avec quelles permissions, ainsi que les conditions d'éligibilité qui restent obligatoires après acceptation.
- [ ] Inscrire explicitement la dépendance de réalisation du publisher et de l'acceptation à la sémantique commune du Step 173.

Déjà consignés au Step 174, donc à conserver : sélection par SHA et GitHub App, nouveau check après résultat terminal, permission `checks:write` à vérifier, erreurs typées, justification/auteur/révocation, invalidation d'une acceptation devenue périmée et migration des anciennes reviews `REQUEST_CHANGES`.

## A05 — Stacks : politique de branche et critères de reprise à préciser

**Origine : PR 724 et 735 ; cas de décodage de la PR 734.**

Le Step **142** contient maintenant membership `stacked`, persistance avant acquittement, dissolution, invalidation immédiate et debounce du lancement par PR. Il reste à compléter :

- [ ] Aligner §17.6, §24.8 et leur articulation avec §39 sur ce cycle de vie.
- [ ] Définir quand une politique de déclenchement examine le parent direct ou la cible ultime de la stack, tout en conservant la base directe pertinente pour le diff incrémental.
- [ ] Inscrire la recette où un nouvel événement arrive pendant une lecture GitHub : le travail devenu obsolète doit être abandonné avant de lancer/publier une review.
- [ ] Inscrire la reprise durable après échec de lecture ou traitement, via les mécanismes persistants existants.
- [ ] Préciser le comportement lorsque la base native est inconnue : une valeur absente ou nulle ne prouve jamais la fraîcheur d'une review.

La PR 735 ne fournit pas un ordonnanceur global de stack ; le Step 142 le précise déjà. La PR 734 ne demande aucun correctif fonctionnel du décodeur Go actuel ; seule la conservation du test de régression reste utile, voir A13.

## A06 — Snapshots : provenance inscrite, compatibilité à spécifier

**Origine : PR 710, 715 et 718.**

Le Step **136** prévoit maintenant la version du runtime au moment de la création du snapshot, sa conservation après restauration et un refus visible. Les absences sont dans le contrat détaillé :

- [ ] Reporter ces règles dans §35 et le cycle de restauration, en distinguant deadline du sandbox et âge/version des binaires restaurés.
- [ ] Définir la source de vérité de la version effectivement embarquée ; une version souhaitée injectée au démarrage ne doit pas réétiqueter un ancien snapshot.
- [ ] Définir la compatibilité attendue et les traitements distincts des snapshots compatibles, incompatibles et sans provenance connue.
- [ ] Préciser le chemin de repli vers une nouvelle lignée, son motif visible et l'articulation avec la continuité/récap du Step 139.
- [ ] Si des snapshots historiques non versionnés existent réellement, documenter leur migration et les différences de capacités entre providers. Un rollout en pourcentage reste conditionnel à ce besoin réel.

Le test de monotonie du plancher de rotation, issu de la PR 709, est déjà inscrit au Step 137.

## A07 — Budget sans progrès : le Step 148 dépasse encore sa spécification

**Origine : PR 717 et 726.**

Le Step **148** distingue désormais budget sans progrès et plafond absolu. **§40 décrit encore principalement les plafonds absolus, le gel, les limites de session et le niveau d'autonomie.**

- [ ] Ajouter le contrat du budget sans progrès : temps actif, coût et tentatives, source des mesures et événements qui réinitialisent la fenêtre.
- [ ] Conserver le progrès vérifiable côté serveur : PR créée/fusionnée confirmée par le fournisseur ou reprise humaine explicite ; une auto-déclaration de l'agent ne suffit pas.
- [ ] Définir quand le contrôle est exercé et ce qu'il fait à un tour déjà actif. Le refus du prochain tour du plafond absolu ne décrit pas, à lui seul, ce nouveau mécanisme.
- [ ] Définir motif d'arrêt/attente, notification et reprise, en cohérence avec les transitions existantes.
- [ ] Préciser qu'une remise à zéro du budget sans progrès ne remet pas à zéro le plafond de dépense ni les bornes absolues du Step 150.

## A08 — UX secrets et réglages : décision non enregistrée

**Origine : PR 686 et 697. Option produit.**

- [ ] Consigner l'adoption, le report ou le rejet d'un accès direct aux secrets utiles juste après création d'une automation.
- [ ] Décrire la distinction visuelle entre réglages personnels et réglages du déploiement, avec les permissions correspondantes.
- [ ] Si le parcours est retenu, préciser comment il réutilise les scopes de secrets existants. Un scope « secret propre à une automation » serait une extension du modèle à décider séparément.

Ces améliorations ne doivent pas être présentées comme déjà couvertes par la seule existence des pages Settings et des secrets.

## A09 — Mode plugin de plateforme : option non enregistrée

**Origine : PR 692 et 702. Option produit.**

- [ ] Consigner la décision sur un mode de plugin fourni par la plateforme et son activation explicite par dépôt.
- [ ] Si des plugins sont fournis, documenter leurs versions épinglées et leur politique de mise à jour.
- [ ] Définir la convergence après restauration : installer la version attendue et retirer les installations/capacités désactivées qui subsistent dans un snapshot.

Le fait qu'un développeur utilise localement un outil de ce type n'établit pas l'existence de ce mode produit dans Narvi. Aucune dépendance obligatoire n'est justifiée par cette comparaison.

## A10 — Médias et quotas : complément non enregistré

**Origine : PR 712 et 725. Option pour la capture ; complément possible pour les uploads.**

Les plafonds d'upload par fichier/session, le contrôle à confirmation et le nettoyage des abandons existent déjà. Restent à décider/documenter :

- [ ] La suppression explicite d'un média déjà prêt et la restitution de son quota, si cette capacité est retenue.
- [ ] Pour un éventuel outil screenshot/vidéo, l'affichage des limites avant de lancer une capture.
- [ ] La relation entre quota d'octets, éventuel quota de places, durée vidéo et échecs/annulations de capture.

Le quota de places upstream ne doit pas être assimilé au système d'uploads existant sans décision produit.

## A11 — Observabilité navigateur : option non enregistrée

**Origine : PR 720, 721, 722 et 723. Option produit/déploiement.**

- [ ] Décider si la capture des erreurs front et le feedback utilisateur complètent le socle OTel/OTLP existant.
- [ ] Si oui, documenter le branchement configurable, les sourcemaps et l'association à la bonne release.
- [ ] Décider séparément du replay et de son masquage, de son activation et de la destination des données selon le déploiement.

Les traces et métriques serveur existantes ne couvrent pas automatiquement ces fonctions navigateur. Un projet de collecte partagé et un replay non masqué ne sont pas des valeurs par défaut à importer.

## A12 — Traçabilité, référence erronée et guides

- [ ] **Publier l'analyse comparative dans la documentation versionnée.** *(Fait depuis — voir la section finale.)*
- [ ] Versionner le présent inventaire. *(Fait depuis.)*
- [ ] **Corriger la référence du Step 176** : il pointe vers §26 (« Review as a merge readout »), alors que le warm boot relève de **§19**. *(Fait depuis.)*
- [ ] Associer chaque lot retenu à la mise à jour des guides correspondants : connexion MCP, trains/Stop, automations sur checks, reviews/acceptation, snapshots et limites. La PR upstream 731 motive cet alignement ; elle ne justifie pas de copier un centre d'aide étranger.
- [ ] Afficher dans les guides la différence entre comportement livré et comportement planifié. L'ajout d'un Step ne prouve pas que la fonctionnalité est disponible.

## A13 — Recettes à conserver explicitement

Cette section complète les critères des lots concernés. Elle ne signifie pas que Narvi n'a aucun test sur ces sujets ; plusieurs protections existent déjà, et les scénarios servent à les préserver lors des reprises.

| Origine | Critère à rattacher au lot |
|---|---|
| PR 734 | Garder un test permanent du vrai décodeur GitHub sur base native absente, nulle et renseignée. La preuve réalisée pendant l'analyse utilisait un overlay temporaire ; elle n'a pas ajouté de test au dépôt. |
| PR 716 | Vérifier la continuité de branche et des modifications locales lors d'une reprise avec stash/checkout/pop. Le mécanisme existe déjà. |
| PR 719, 730 | Vérifier sur une restauration réelle l'égalité des capacités avec une création neuve et le retrait des anciens outils désactivés. Le partage de `SessionConfig` couvre déjà une partie de cet invariant. |
| PR 730, MCP | Révision de plan mise en file pendant un travail approuvé ; lectures distinguant pending/processing/approbation ; arrêt et attente bornée. |
| PR 685–689, 730 | Dépendances héritées vers feuilles, message vers la bonne session, Stop propagé, enfant terminé sans PR et notification idempotente. |
| PR 727 | Vérifier le head/diff de l'ancrage des findings et le résultat effectivement publié. `MatchPosition` et le fallback LLM existent ; aucun portage AST systématique n'est requis. |
| PR 730, uploads | Préserver l'impossibilité d'utiliser une clé média forgée ou celle d'une autre session. La construction serveur des clés et la lecture bornée existent déjà. |
| PR 730, provider | Préserver le rejet des résultats d'une ancienne génération de provider. Le filtrage existe déjà. |

Déjà inscrits par #289 et à exécuter avec leur lot : monotonie de rotation (137), invalidation immédiate/debounce par PR (142), progression vérifiable (148), contexte/tentative (173–174), assemblage SSE réel et reconnexion (175).

## Ce qui était déjà sur main et ne doit pas être recréé

| Reprise | Inscription existante | Ce qui reste dans cet inventaire |
|---|---|---|
| Dispatch réel des automations, puis check CI nommé | Step 172 | Guide et recette lors de la livraison |
| Contexte de review et `not_assessed` | Step 173 | A04 : spécification et consommateurs |
| Publisher GitHub Checks et acceptation humaine | Step 174 | A04 : politique et parcours détaillés |
| Runtime OpenCode, catalogue, image et deltas SSE cohérents | Step 175 | Recette déjà demandée |
| Motif du choix d'image et manifests modifiés | Step 176 | A12 : référence de section erronée |
| Version/provenance du snapshot | Step 136 | A06 : contrat de compatibilité |
| Plancher de rotation et test de monotonie | Step 137 | Pas d'omission spécifique restante |
| Événements stacked, retarget, invalidation et debounce | Step 142 | A05 : politique et cas de concurrence/reprise |
| Budget sans progrès distinct des plafonds absolus | Step 148 | A07 : spécification détaillée |
| Bornes absolues de session | Step 150 | À articuler avec A07, sans les dupliquer |

## Ordre conseillé pour fermer les absences documentaires

1. Aligner les spécifications sur les reprises déjà inscrites : A04–A07, puis corriger la référence A12.
2. Compléter les trains et enregistrer le besoin de diagnostic : A02–A03.
3. Consigner une décision MCP explicite et, si retenu, son lot complet A01 avec les critères A13.
4. Enregistrer adoption, report ou rejet pour A08–A11, afin que ces options ne disparaissent pas implicitement du suivi.
5. Versionner l'analyse et cet inventaire ; rattacher guides et recettes aux lots concernés.

## Ce qui a bougé depuis la rédaction

Ce document a été contrôlé contre `main` à `071f1072`. Les points suivants ont été fermés après, ce qui illustre pourquoi un inventaire épingle son commit :

- **A04, premier point** — l'amendement de §21.1 et §39.3 a été écrit et appliqué. §21.1 distingue désormais son résidu accepté (un défaut *manqué*) du danger réel (un verdict *présenté comme courant* alors que son contexte a changé), et §39.3 date explicitement sa propre limite tant que le Step 173 n'est pas livré.
- **A12, référence du Step 176** — corrigée de §26 vers §19. Ce constat a mis au jour une limite de l'outillage : le contrôle de citations de `internal/ops` vérifie qu'un `§N` *résout*, jamais qu'il désigne la bonne section, et il passait donc sur les deux.
- **A12, publication** — les deux analyses sont versionnées sous `docs/analyses/`, avec un README expliquant leur statut non normatif.
- **A05** — §24.8 porte désormais le cycle de vie de l'appartenance à une stack : invalidation immédiate contre lancement différé, debounce par PR annoncé comme tel, persistance avant acquittement, abandon d'une tentative obsolète, et « base inconnue » comme troisième état qui ne vaut jamais fraîcheur. La question parent direct contre cible ultime y est nommée comme décision ouverte plutôt que tranchée en silence.
- **A06** — §35.5b énonce le contrat de compatibilité des snapshots : provenance capturée au mint, source de vérité étant ce que le snapshot contient et non une version souhaitée injectée au boot, et trois états jamais confondus (compatible, incompatible, provenance inconnue).
- **A07** — §40.3 distingue désormais borner le travail écoulé de borner le travail gâché, avec le progrès observable défini comme vérifiable côté serveur, et l'indépendance explicite entre ce budget et les plafonds absolus.

- **A02** — les Steps 145 à 147 et le §39 portent maintenant ce qui manquait au train : décomposition jusqu'aux feuilles en conservant l'ascendance, conteneurs sans session de code, blocages hérités résolus vers les feuilles, réconciliation du graphe soumis avec les relations vivantes du tracker avant tout lancement (§39.2), et un §39.5b sur le lien comme identité durable — association feuille/lien/session persistée, prédécesseur et base résolus au démarrage et non à la soumission, fin d'un enfant **sans PR** avec sortie terminale propre au lieu d'une attente infinie de verdict, Stop atteignant aussi les liens en attente, et retrait du routage des messages.
- **A03** — le §7.3 et le **Step 177** inscrivent le diagnostic runtime/fournisseur : champs conservés par liste blanche et jamais l'objet d'erreur entier, plafond de taille, restitution unique lue depuis le journal de session et le chemin opérateur, décision de retry inchangée sur le discriminant typé. Le critère de sortie exige les deux moitiés — retrouver la cause **et** vérifier l'absence de credentials et de contenu de requête.

- **A01 et A08 à A11** — les cinq décisions produit sont désormais inscrites dans `docs/DECISIONS.md`, avec pour chacune la question, pourquoi elle ne peut pas être tranchée par défaut, ce que coûte l'adoption et ce que gate le report. Elles y sont **posées, pas prises** : le registre appartient au propriétaire du produit. Le même fichier indexe les décisions déjà rendues et celles qu'une ligne de plan porte déjà, sans les recopier — une décision consignée deux fois est une décision qui divergera.

- **A12, volet guides** — `docs/guides/README.md` porte désormais les deux règles et la table « quel lot doit quel guide ». La proposition de marqueur `[planned §N]` y est **rejetée avec sa raison** : ce modèle convient à `FOUNDATIONS.md`, qui dit ce que le système est, et pas à un guide lu par quelqu'un qui essaie de faire quelque chose maintenant — une entrée « planifié » y est une instruction qui échoue. Le **Step 178** inscrit la direction que le contrôle de dérive ne parcourt pas : il refuse un guide qui documente une commande inexistante, jamais un guide qui en omet une qui existe.
- **A13** — les recettes qui protègent du code déjà livré ont été rattachées au lot qui **adopte**, pas à celui qui a écrit le code : les rattacher à une ligne livrée ne protège rien, puisque personne ne la relit. Le Step 175 (montée du runtime, du catalogue et de l'image) porte donc les trois protections que cette montée pourrait faire disparaître — parité des capacités après restauration avec retrait des outils désactivés, refus d'une clé média forgée ou d'une autre session, rejet d'un résultat d'ancienne génération de provider. Les recettes issues des PR 716 et 727 protègent du comportement livré dont aucun lot n'est aujourd'hui l'adoptant ; elles restent sans porteur, et ce constat vaut mieux qu'un rattachement décoratif à une ligne close.

- **A04** — écrit après la livraison du Step 173 (PR #294, neuf tours d'audit, 79 défauts confirmés), contre le code réellement livré et non contre l'intention. Le §21.1b énonce les quatre absences restantes : tous les consommateurs — inbox, éligibilité, auto-merge, garde de train, publisher — lisent le MÊME contexte persisté et la même tentative, et aucun ne recalcule le sien, faute de quoi il devient une seconde autorité qui dérive en silence ; une émission porte la tentative et le contexte pour lesquels elle a été produite et est refusée si l'un des deux est dépassé, puisque l'égalité de head ne suffit pas à identifier un résultat ; une course de création rend UNE identité, deux identités actives valant pire que zéro ; `not_assessed` reste à trancher côté GitHub, dont le vocabulaire n'a pas de valeur signifiant « non évalué » et dont `neutral` satisfait un required check ; et l'acceptation humaine se lie à un verdict, une tentative et un contexte, ne réécrit jamais l'évaluation, et laisse obligatoires les conditions qui ne relèvent pas du jugement humain. La dépendance de réalisation du publisher au Step 173 est inscrite comme un ordre, pas une préférence.

Reste ouvert à la date de cette mise à jour : les cinq décisions produit de `docs/DECISIONS.md`, qui appartiennent au propriétaire, et les deux recettes sans porteur ci-dessus.
